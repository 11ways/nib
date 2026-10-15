package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
)

// ordered is a JSON object that keeps its keys in order, so rewriting a config changes nothing but what setup adds.
type ordered struct {
	keys   []string
	values map[string]any
}

func newOrdered() *ordered { return &ordered{values: map[string]any{}} }

func (o *ordered) get(key string) (any, bool) {
	value, ok := o.values[key]
	return value, ok
}

// set replaces a key's value in place, or adds the key at the end.
func (o *ordered) set(key string, value any) {
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// remove takes a key out, keeping the others in order.
func (o *ordered) remove(key string) {
	if _, ok := o.values[key]; !ok {
		return
	}
	delete(o.values, key)
	o.keys = slices.DeleteFunc(o.keys, func(k string) bool { return k == key })
}

// decode parses JSON into ordered objects, lists, json.Number, strings, booleans and nil.
func decode(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return value, nil
}

func decodeValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch t := token.(type) {
	case json.Delim:
		switch t {
		case '{':
			object := newOrdered()
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				value, err := decodeValue(decoder)
				if err != nil {
					return nil, err
				}
				object.set(key.(string), value)
			}
			_, err := decoder.Token()
			return object, err
		case '[':
			list := []any{}
			for decoder.More() {
				value, err := decodeValue(decoder)
				if err != nil {
					return nil, err
				}
				list = append(list, value)
			}
			_, err := decoder.Token()
			return list, err
		}
		return nil, errors.New("unexpected delimiter")
	}
	return token, nil
}

// encode writes a value as JSON.stringify(value, null, 2) does.
func encode(value any) string {
	var b strings.Builder
	write(&b, value, "")
	return b.String()
}

func write(b *strings.Builder, value any, indent string) {
	inner := indent + "  "
	switch v := value.(type) {
	case *ordered:
		if len(v.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, key := range v.keys {
			b.WriteString(inner)
			b.WriteString(quoteJSON(key))
			b.WriteString(": ")
			write(b, v.values[key], inner)
			if i < len(v.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	case []any:
		if len(v) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, item := range v {
			b.WriteString(inner)
			write(b, item, inner)
			if i < len(v)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "]")
	case string:
		b.WriteString(quoteJSON(v))
	case json.Number:
		b.WriteString(v.String())
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case nil:
		b.WriteString("null")
	case int:
		data, _ := json.Marshal(v)
		b.Write(data)
	default:
		data, _ := json.Marshal(v)
		b.Write(data)
	}
}

func quoteJSON(s string) string {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}
