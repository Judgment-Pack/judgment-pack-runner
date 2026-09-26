package derivation

import "errors"

// These entry points accept already bounded JSON supplied by Runner. The
// retained reference implementation and its agreement semantics are unchanged.
func Canon(raw []byte) ([]byte, error) {
	v, err := parseJSON(raw)
	if err != nil {
		return nil, err
	}
	return canon(v)
}
func Validate(raw []byte) error {
	v, err := parseJSON(raw)
	if err != nil {
		return err
	}
	_, err = validateRule(v)
	return err
}
func Derive(rule, artifact, parameters []byte) ([]byte, error) {
	r, err := parseJSON(rule)
	if err != nil {
		return nil, err
	}
	a, err := parseJSON(artifact)
	if err != nil {
		return nil, err
	}
	p, err := parseJSON(parameters)
	if err != nil {
		return nil, err
	}
	o, ok := p.(Obj)
	if !ok {
		return nil, errors.New("parameters must be an object")
	}
	return derive(r, a, o)
}
