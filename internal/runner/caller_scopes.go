package runner

// Caller scopes (runner #304, docs/adr/0002-caller-scopes.md): a caller is
// held to the scope identifiers its verified identity allows. Its token names
// the scope identifiers it is for in the claim scope_ids; the service
// configuration's identity.callerScopes names, per subject, those the subject
// may be granted. A request reaches what both allow; a token that names one
// its subject's entry does not grant is refused, never narrowed, so that a
// mis-issued token is seen. A token with no claim reaches the records under
// no scope identifier, or, for a subject whose entry is "installation", every
// scope and the installation's routes. The owner bearer is the
// installation's. The gateway (its #479) reads the same claim.

import (
	"context"
	"errors"
	"net/http"
)

// scopeClaim is the claim of a token that names the scope identifiers it is
// for, the gateway's as well as the Runner's.
const scopeClaim = "scope_ids"

// maxCallerScopes bounds the scope identifiers a claim names, and those an
// entry grants.
const maxCallerScopes = 256

// unheldScope is a scope no record carries: outside the scope identifier's
// form (a control character), so no record is ever created under it. A
// request that names a scope its caller does not reach is held to it in that
// scope's place, so that it reads and writes no scoped record on any kind,
// and a listing filtered by it keeps none (scope.go).
const unheldScope = "\x01unheld"

// callerEntry is one subject's entry: the scope identifiers it may be
// granted, or every one, the installation's. The zero value is "unscoped":
// none.
type callerEntry struct {
	installation bool
	scopes       map[string]bool
}

// grants says whether the entry grants the scope identifier scope.
func (e callerEntry) grants(scope string) bool { return e.installation || e.scopes[scope] }

// callerScopes is identity.callerScopes as read. The zero value is checked
// with no entry: every subject unscoped (C9).
type callerScopes struct {
	// unchecked keeps the behavior before #304: the claim is not read, and
	// every caller reaches what the installation reaches.
	unchecked bool
	// entries are the subjects' entries; a listed subject without one is
	// unscoped.
	entries map[string]callerEntry
}

// errCallerScopesForm and errCallerScopesSubject are the refusals of a
// callerScopes member Runner does not take. Neither repeats a value.
var (
	errCallerScopesForm    = errors.New(`identity.callerScopes must be "unchecked", or an object whose members are subjects identity.subjects lists, each naming the scope identifiers it may be granted (1 to 256 of them, each once), "unscoped" or "installation"`)
	errCallerScopesSubject = errors.New("identity.callerScopes names a subject identity.subjects does not list")
)

// readCallerScopes reads identity.callerScopes: "unchecked", or an object
// whose names are subjects the identity lists, each with an array of 1 to 256
// scope identifiers (each once, each in form), "unscoped" or
// "installation". A refusal names the member, never a value.
func readCallerScopes(v any, subjects []string) (callerScopes, error) {
	if v == "unchecked" {
		return callerScopes{unchecked: true}, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return callerScopes{}, errCallerScopesForm
	}
	listed := map[string]bool{}
	for _, subject := range subjects {
		listed[subject] = true
	}
	c := callerScopes{entries: map[string]callerEntry{}}
	for subject, raw := range obj {
		if !listed[subject] {
			return callerScopes{}, errCallerScopesSubject
		}
		var e callerEntry
		switch raw {
		case "unscoped":
		case "installation":
			e.installation = true
		default:
			scopes, ok := scopeList(raw)
			if !ok {
				return callerScopes{}, errCallerScopesForm
			}
			e.scopes = scopes
		}
		c.entries[subject] = e
	}
	return c, nil
}

// scopeList reads an array of 1 to 256 scope identifiers, each once, each in
// form, as a set.
func scopeList(raw any) (map[string]bool, bool) {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 || len(list) > maxCallerScopes {
		return nil, false
	}
	scopes := map[string]bool{}
	for _, item := range list {
		scope, ok := item.(string)
		if !ok || !isScopeIdentifier(scope) || scopes[scope] {
			return nil, false
		}
		scopes[scope] = true
	}
	return scopes, true
}

// errScopeClaimForm and errScopeClaimWider are the refusals of a token whose
// claim is out of form, or names a scope identifier its subject's entry does
// not grant. Neither repeats any part of the token.
var (
	errScopeClaimForm  = errors.New("the token's " + scopeClaim + " is not 1 to 256 scope identifiers, each once")
	errScopeClaimWider = errors.New("the token's " + scopeClaim + " names a scope identifier the configuration does not grant its subject")
)

// reachOf is what a token for subject, with claims, reaches: with the check
// unchecked, every scope; with no claim, the none scope, or every scope for
// an "installation" entry; with a claim, the scope identifiers the claim and
// the subject's entry both allow, and a claim that names one the entry does
// not grant is refused rather than narrowed.
func (c callerScopes) reachOf(subject string, claims map[string]any) (callerReach, error) {
	if c.unchecked {
		return installationReach, nil
	}
	entry := c.entries[subject]
	raw, present := claims[scopeClaim]
	if !present {
		if entry.installation {
			return installationReach, nil
		}
		return callerReach{none: true}, nil
	}
	claimed, ok := scopeList(raw)
	if !ok {
		return callerReach{}, errScopeClaimForm
	}
	// The effective scopes are the claim's and the entry's both. A claim
	// wider than the entry is refused, not narrowed to it.
	effective := map[string]bool{}
	for scope := range claimed {
		if entry.grants(scope) {
			effective[scope] = true
		}
	}
	if len(effective) != len(claimed) {
		return callerReach{}, errScopeClaimWider
	}
	return callerReach{scopes: effective}, nil
}

// mode is GET /v1/status's callerScopes: "checked" or "unchecked".
func (c callerScopes) mode() string {
	if c.unchecked {
		return "unchecked"
	}
	return "checked"
}

// CallerScopesNote is the line the start writes on standard error when caller
// scopes are unchecked, and "" when they are checked.
func (id *Identity) CallerScopesNote() string {
	if id == nil || !id.scopes.unchecked {
		return ""
	}
	return "caller scopes are unchecked (identity.callerScopes): every subject the identity lists may name any scope identifier, list every scope's records and call the installation's routes, and a token's " + scopeClaim + " is not read"
}

// callerReach is what a request's caller may name: the installation's every
// scope, or the scope identifiers its checked identity allows, or the none
// scope. The zero value reaches nothing: a request that carries none is held
// to no record.
type callerReach struct {
	// installation is every scope, the store-wide listings unfiltered and
	// the installation's routes, as the owner bearer.
	installation bool
	// none is the none scope: the records created under no scope identifier.
	none bool
	// scopes are the scope identifiers reached.
	scopes map[string]bool
}

// installationReach is the owner bearer's, and an unchecked caller's.
var installationReach = callerReach{installation: true}

// reaches says whether the caller may name scope, "" being the none scope.
func (c callerReach) reaches(scope string) bool {
	switch {
	case c.installation:
		return true
	case scope == "":
		return c.none
	}
	return c.scopes[scope]
}

// callerReachContext is the key of a request's caller reach in its context.
type callerReachContext struct{}

// withCallerReach is ctx carrying reach as its request's caller's.
func withCallerReach(ctx context.Context, reach callerReach) context.Context {
	return context.WithValue(ctx, callerReachContext{}, reach)
}

// callerReachFrom is the reach ctx carries, which reaches nothing when it
// carries none.
func callerReachFrom(ctx context.Context) callerReach {
	reach, _ := ctx.Value(callerReachContext{}).(callerReach)
	return reach
}

// errScopeNotAllowed refuses a job created under a scope identifier, or under
// none, that its caller does not reach. It depends on no stored record and
// names no scope.
var errScopeNotAllowed = &apiError{http.StatusForbidden, "scope_not_allowed", "This caller's identity does not allow a job under the scope identifier named, or under none when none is named."}

// errInstallationCallerRequired refuses an installation's route to a caller
// held to scopes: the route acts for every scope identifier.
var errInstallationCallerRequired = &apiError{http.StatusForbidden, "installation_caller_required", "This route acts for every scope identifier: it is served only to the installation's caller."}
