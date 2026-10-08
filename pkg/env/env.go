// Package env is an insertion-ordered set of environment variables.
package env

import "encoding/json"

// Env is an insertion-ordered set of environment variables. Setting an existing name keeps its
// position, so rendered task definitions keep a stable order.
type Env struct {
	keys []string
	vals map[string]string
}

func New() *Env { return &Env{vals: map[string]string{}} }

func (e *Env) Set(name, value string) {
	if _, ok := e.vals[name]; !ok {
		e.keys = append(e.keys, name)
	}
	e.vals[name] = value
}

func (e *Env) Get(name string) (string, bool) {
	v, ok := e.vals[name]
	return v, ok
}

func (e *Env) Keys() []string { return append([]string{}, e.keys...) }

func (e *Env) Len() int { return len(e.keys) }

// Merge sets every variable of o, in o's order.
func (e *Env) Merge(o *Env) {
	for _, k := range o.keys {
		e.Set(k, o.vals[k])
	}
}

// Map is a plain copy (order lost).
func (e *Env) Map() map[string]string {
	m := make(map[string]string, len(e.vals))
	for k, v := range e.vals {
		m[k] = v
	}
	return m
}

// NameValue is the ECS / compose list form.
type NameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (e *Env) Pairs() []NameValue {
	out := make([]NameValue, 0, len(e.keys))
	for _, k := range e.keys {
		out = append(out, NameValue{k, e.vals[k]})
	}
	return out
}

// MarshalJSON renders an ordered JSON object.
func (e *Env) MarshalJSON() ([]byte, error) {
	b := []byte{'{'}
	for i, k := range e.keys {
		if i > 0 {
			b = append(b, ',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(e.vals[k])
		b = append(append(append(b, kb...), ':'), vb...)
	}
	return append(b, '}'), nil
}

// Delete removes name.
func (e *Env) Delete(name string) {
	if _, ok := e.vals[name]; !ok {
		return
	}
	delete(e.vals, name)
	for i, k := range e.keys {
		if k == name {
			e.keys = append(e.keys[:i], e.keys[i+1:]...)
			break
		}
	}
}
