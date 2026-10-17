package runner

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
)

// The input profiles a preparation verifier is given (runner #124).
//
// spec/dialect.md §5.1 states a profile's members and their forms, and "Its
// digest is the digest of its canon bytes". The verifier holds the profiles
// it is given to that, and to the bounds of §12, alone, as the
// specification's reader does (tools/dialect_verify.py, parse_profiles), and
// digests each as given. An installation's profiles are admitted otherwise
// (validateProfileKeys): Runner holds them to limits of its own, at most 16
// profiles and 32 KiB each, an id of a name's form, a source, an authority and
// an adapter's name and version written non-empty and bounded, an endpoint
// and a tool allowlist bounded; and it digests the profile it admitted, as it
// writes it, in which a tools member written empty is absent. Those limits and
// that digest stay the installation's: a live preparation, an import and
// verify-run keep them.

// verifierProfiles reads the profiles a verifier is given, one JSON array
// already read as §1 reads a document (document is its value, raw its bytes),
// as §5.1 states each profile: an object of exactly id, publicKey, class,
// source, authority, shape, adapter and endpoint, and optionally tools,
// calculator and research, each of its form (profileForm). Each profile it
// returns carries the digest of its canon bytes as given, which profileHash
// names for it.
func verifierProfiles(document any, raw []byte) ([]InputProfile, error) {
	list, ok := document.([]any)
	var elements []json.RawMessage
	if !ok || json.Unmarshal(raw, &elements) != nil || len(elements) != len(list) {
		return nil, errors.New("they are not one JSON array of profiles")
	}
	ids := map[string]bool{}
	profiles := make([]InputProfile, 0, len(list))
	for i, value := range list {
		if err := profileForm(value, ids); err != nil {
			return nil, fmt.Errorf("profile %d: %w", i, err)
		}
		canon, err := proofCanon(elements[i])
		if err != nil {
			return nil, fmt.Errorf("profile %d is outside the gateway canonical domain", i)
		}
		var p InputProfile
		if err := strictJSON(elements[i], &p); err != nil {
			return nil, fmt.Errorf("profile %d: %w", i, err)
		}
		p.canonDigest = digest(canon)
		profiles = append(profiles, p)
	}
	return profiles, nil
}

// profileForm holds one profile to §5.1's table: its members exactly, an id
// that is a non-empty string distinct among the profiles (ids holds those
// before it), a key the key rule admits, a class and a shape of §5.1's
// values, source and authority strings, an adapter of exactly name and
// version, strings, and digest, a digest, an endpoint that is a string or
// null, tools, when given, distinct non-empty strings, which a profile of
// shape mcp gives and not empty, a calculator of exactly name and version,
// strings, on a profile of class record and shape mcp, and a research
// declaration of §5.1's form. Its maxima, three capabilities and a model of
// 256 bytes, are counted before any form (preparationCountBounds).
func profileForm(value any, ids map[string]bool) error {
	p, ok := value.(map[string]any)
	if !ok || !closedMembers(p, []string{"id", "publicKey", "class", "source", "authority", "shape", "adapter", "endpoint"}, []string{"tools", "calculator", "research"}) {
		return errors.New("a profile is an object of exactly id, publicKey, class, source, authority, shape, adapter and endpoint, and optionally tools, calculator and research")
	}
	str := func(v any) bool { _, ok := v.(string); return ok }
	id, _ := p["id"].(string)
	if id == "" || ids[id] {
		return errors.New("its id is not a non-empty string distinct among the profiles")
	}
	ids[id] = true
	key, _ := p["publicKey"].(string)
	if _, err := ParsePublicKey(key); err != nil {
		return fmt.Errorf("its publicKey: %w", err)
	}
	if p["class"] != "record" && p["class"] != "generated" {
		return errors.New("its class is not record or generated")
	}
	if !str(p["source"]) || !str(p["authority"]) {
		return errors.New("its source and authority are not strings")
	}
	shape, _ := p["shape"].(string)
	if !adapterShapes[shape] && shape != "command" {
		return errors.New("its shape is not airbyte, mcp, http or command")
	}
	adapter, _ := p["adapter"].(map[string]any)
	if !closedMembers(adapter, []string{"name", "version", "digest"}, nil) || !str(adapter["name"]) || !str(adapter["version"]) {
		return errors.New("its adapter is not an object of exactly name, version and digest, name and version strings")
	}
	if pinned, _ := adapter["digest"].(string); !validDigest(pinned) {
		return errors.New("its adapter's digest is not a digest")
	}
	if endpoint := p["endpoint"]; endpoint != nil && !str(endpoint) {
		return errors.New("its endpoint is not a string or null")
	}
	var tools []any
	if value, present := p["tools"]; present {
		list, ok := value.([]any)
		seen := map[string]bool{}
		for _, tool := range list {
			name, _ := tool.(string)
			ok = ok && name != "" && !seen[name]
			seen[name] = true
		}
		if !ok {
			return errors.New("its tools are not an array of distinct non-empty strings")
		}
		tools = list
	}
	if shape == "mcp" && len(tools) == 0 {
		return errors.New("it is of shape mcp, and gives no tools")
	}
	if calculator, present := p["calculator"]; present {
		c, _ := calculator.(map[string]any)
		if !closedMembers(c, []string{"name", "version"}, nil) || !str(c["name"]) || !str(c["version"]) {
			return errors.New("its calculator is not an object of exactly name and version, strings")
		}
		if p["class"] != "record" || shape != "mcp" {
			return errors.New("it is a calculator's, and not of class record and shape mcp")
		}
	}
	if research, present := p["research"]; present {
		r, _ := research.(map[string]any)
		if !closedMembers(r, []string{"capabilities"}, []string{"model"}) {
			return errors.New("its research declaration is not an object of capabilities and, optionally, model")
		}
		capabilities, ok := checkCapabilities(r["capabilities"])
		if !ok {
			return errors.New("its research capabilities are not distinct names of answer, search and page, in that order, one at least")
		}
		model, named := r["model"]
		if asks(capabilities, "answer") != named {
			return errors.New("its research declaration does not name a model exactly when it serves answer")
		}
		if text, _ := model.(string); named && text == "" {
			return errors.New("its research model is not a non-empty string")
		}
	}
	return nil
}

// verifierProfileKeys is each profile's key, by its id, for profiles held to
// §5.1 when the verifier read them (verifierProfiles): the key rule is the
// only check here, and Runner's own limits on an installation's profiles
// (validateProfileKeys) are not applied.
func verifierProfileKeys(profiles []InputProfile) (map[string]ed25519.PublicKey, error) {
	keys := map[string]ed25519.PublicKey{}
	for _, p := range profiles {
		key, err := ParsePublicKey(p.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("invalid input profile public key: %w", err)
		}
		keys[p.ID] = key
	}
	return keys, nil
}
