package runner

import (
	"encoding/json"
	"errors"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/retainedcolumns"
)

func columnReferenceForms(preparation map[string]any) error {
	lineage, _ := preparation["lineage"].([]any)
	for _, entry := range lineage {
		target, _ := entry.(map[string]any)
		raw, exists := target["columnTransformation"]
		if !exists {
			continue
		}
		transformation, ok := raw.(map[string]any)
		if !ok {
			return errors.New("column transformation must be an object")
		}
		var input retainedcolumns.InputState
		if json.Unmarshal(encode(transformation["input"]), &input) != nil {
			return errors.New("invalid closed retained-column input reference")
		}
	}
	return nil
}
