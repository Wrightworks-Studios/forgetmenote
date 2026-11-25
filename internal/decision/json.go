package decision

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
)

// Reject duplicate object keys as well as trailing documents and unknown fields.
func strictJSON(raw []byte, out any) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	if !uniqueValue(d, 0) {
		return false
	}
	if _, err := d.Token(); err != io.EOF {
		return false
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if !exactTypedFields(raw, reflect.TypeOf(out)) {
		return false
	}
	return d.Decode(out) == nil && d.Decode(new(any)) == io.EOF
}
func uniqueValue(d *json.Decoder, depth int) bool {
	if depth > 64 {
		return false
	}
	t, err := d.Token()
	if err != nil {
		return false
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return true
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			t, err = d.Token()
			k, ok := t.(string)
			if err != nil || !ok || seen[k] {
				return false
			}
			seen[k] = true
			if !uniqueValue(d, depth+1) {
				return false
			}
		}
	case '[':
		for d.More() {
			if !uniqueValue(d, depth+1) {
				return false
			}
		}
	default:
		return false
	}
	_, err = d.Token()
	return err == nil
}

// encoding/json accepts case aliases for struct fields. Require declared names
// at typed boundaries while leaving user-defined map keys case-sensitive.
func exactTypedFields(raw []byte, t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeFor[json.RawMessage]() || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return false
		}
		declared := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			declared[name] = field.Type
		}
		for name, value := range fields {
			field, ok := declared[name]
			if !ok || !exactTypedFields(value, field) {
				return false
			}
		}
	case reflect.Map:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return false
		}
		for _, value := range fields {
			if !exactTypedFields(value, t.Elem()) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return false
		}
		for _, value := range values {
			if !exactTypedFields(value, t.Elem()) {
				return false
			}
		}
	}
	return true
}
