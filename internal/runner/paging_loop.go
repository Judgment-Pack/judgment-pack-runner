package runner

// The run path of a paged source (docs/design/paging.md, section 4): a
// dialect of format 4.1.0 names the pages' rule (RFC 0033), and a job names,
// for each of its paged sources, the cursor kept between runs. An occurrence
// fills the cursor into the case, acquires the source's pages in one gateway
// session, each request after the first formed from the page before, and
// carries the cursor's advance into its run; the run that completes writes
// it (finishRunIn), under its claim and only while the cursor still holds
// what the occurrence read.

import (
	"encoding/json"
	"fmt"
)

// plannedPaging is a paged source's rule and read, which the loop forms each
// request and reads each page's next reference with.
type plannedPaging struct {
	rule *pagingRule
	read SourceRead
}

// readPage reads page i's value as the walk does (RFC 0033 section 4, step
// 5): the read's unwrap, then an items array at items and a next reference
// spelled as noNext declares, read-failed otherwise. next is "" at a page
// that names none.
func readPage(read SourceRead, rule *pagingRule, result json.RawMessage, i int) (any, []any, string, error) {
	itemTokens, _ := pointerTokens(rule.Items, true)
	nextTokens, _ := pointerTokens(rule.Next, false)
	parent, member := nextTokens[:len(nextTokens)-1], nextTokens[len(nextTokens)-1]
	artifact, err := readInputJSON(result)
	if err != nil {
		return nil, nil, "", withCode(codeReadFailed, err)
	}
	v, err := unwrapRead(SourceRead{Unwrap: read.Unwrap}, artifact)
	if err != nil {
		return nil, nil, "", err
	}
	pageItems, ok := pointerRead(v, itemTokens)
	list, isList := pageItems.([]any)
	if !ok || !isList {
		return nil, nil, "", withCode(codeReadFailed, fmt.Errorf("page %d holds no items array at %s", i, rule.Items))
	}
	holder, ok := pointerRead(v, parent)
	object, isObject := holder.(map[string]any)
	if !ok || !isObject {
		return nil, nil, "", withCode(codeReadFailed, fmt.Errorf("page %d holds no object where %s names its next reference", i, rule.Next))
	}
	reference, named := object[member]
	switch text, isText := reference.(string); {
	case !named && rule.NoNext == "absent", named && reference == nil && rule.NoNext == "null", isText && text == "" && rule.NoNext == "empty-string":
		return v, list, "", nil
	case isText && text != "":
		if len(text) > maxReferenceBytes {
			return nil, nil, "", withCode(codeOverBound, fmt.Errorf("page %d names a next reference over %d bytes", i, maxReferenceBytes))
		}
		return v, list, text, nil
	}
	return nil, nil, "", withCode(codeReadFailed, fmt.Errorf("page %d's next reference is neither a string nor the end noNext declares", i))
}
