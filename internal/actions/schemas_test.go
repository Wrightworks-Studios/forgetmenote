package actions

import "testing"

func TestSchemasAreBuiltOncePerTypePair(t *testing.T) {
	type input struct {
		Name string `json:"name"`
	}
	type other struct {
		Title string `json:"title"`
	}
	type output struct {
		Done bool `json:"done"`
	}
	schema, result, resolved := Schemas[input, output]()
	again, resultAgain, resolvedAgain := Schemas[input, output]()
	if schema != again || result != resultAgain || resolved != resolvedAgain {
		t.Fatal("the same types built their schemas twice")
	}
	if resolved.Validate(map[string]any{"name": "a"}) != nil {
		t.Fatal("a shared schema refused valid arguments")
	}
	different, _, _ := Schemas[other, output]()
	if different.Properties["title"] == nil || different.Properties["name"] != nil {
		t.Fatalf("other types got the wrong schema: %+v", different.Properties)
	}
}
