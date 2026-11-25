package catalog

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// Populate every exported field so newly added mutable fields also participate
// in the isolation check rather than silently escaping a hand-picked fixture.
func populateCatalogValue(v reflect.Value, marker string) {
	if v.Type() == reflect.TypeOf(time.Time{}) {
		v.Set(reflect.ValueOf(time.Unix(123, 0).UTC()))
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		populateCatalogValue(v.Elem(), marker)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				populateCatalogValue(v.Field(i), marker)
			}
		}
	case reflect.Map:
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		key := reflect.ValueOf("key").Convert(v.Type().Key())
		item := reflect.New(v.Type().Elem()).Elem()
		if existing := v.MapIndex(key); existing.IsValid() {
			item.Set(existing)
		}
		populateCatalogValue(item, marker)
		v.SetMapIndex(key, item)
	case reflect.Slice:
		if v.Len() == 0 {
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		}
		for i := 0; i < v.Len(); i++ {
			populateCatalogValue(v.Index(i), marker)
		}
	case reflect.String:
		v.SetString(marker)
	case reflect.Bool:
		v.SetBool(marker == "before")
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if marker == "before" {
			v.SetUint(1)
		} else {
			v.SetUint(2)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if marker == "before" {
			v.SetInt(1)
		} else {
			v.SetInt(2)
		}
	}
}
func TestCatalogTransactionCloneIsolatesEveryMutableField(t *testing.T) {
	var original catalogData
	populateCatalogValue(reflect.ValueOf(&original).Elem(), "before")
	// Arguments are opaque JSON in production, not numeric byte fixtures.
	for id, assistant := range original.Assistants {
		assistant.Watches[0].Arguments = json.RawMessage(`{"scope":"before"}`)
		original.Assistants[id] = assistant
	}
	before, _ := json.Marshal(original)
	copied := cloneCatalog(original)
	if !reflect.DeepEqual(original, copied) {
		t.Fatal("clone changed catalog values")
	}
	populateCatalogValue(reflect.ValueOf(&copied).Elem(), "after")
	after, err := json.Marshal(original)
	if err != nil || string(before) != string(after) {
		t.Fatalf("transaction leaked into original state: %v", err)
	}
	var empty catalogData
	if !reflect.DeepEqual(empty, cloneCatalog(empty)) {
		t.Fatal("clone changed nil fields")
	}
}
