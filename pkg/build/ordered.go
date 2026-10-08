package build

import (
	"bytes"
	"encoding/json"
)

// KV is one entry of an ordered JSON object of hashes.
type KV struct{ Key, Value string }

// OrderedHashes marshals as a JSON object in slice order.
type OrderedHashes []KV

func (o OrderedHashes) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv.Key)
		v, _ := json.Marshal(kv.Value)
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (o *OrderedHashes) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if _, err := dec.Token(); err != nil {
		return err
	}
	*o = OrderedHashes{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			return err
		}
		*o = append(*o, KV{kt.(string), v})
	}
	return nil
}

// Map is a plain lookup copy.
func (o OrderedHashes) Map() map[string]string {
	m := make(map[string]string, len(o))
	for _, kv := range o {
		m[kv.Key] = kv.Value
	}
	return m
}
