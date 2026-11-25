package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

var errStrictSchema = errors.New("tool schema is incompatible with configured strict mode")

// StrictToolDefinitions builds private wire schemas. The shared action schemas
// keep their original required fields and defaults for MCP and execution.
func StrictToolDefinitions(tools []Tool) ([]Tool, error) {
	out := append([]Tool(nil), tools...)
	for i := range out {
		var schema map[string]any
		if json.Unmarshal(out[i].Schema, &schema) != nil || schema["type"] != "object" {
			return nil, errStrictSchema
		}
		if err := makeStrictSchema(schema, schema, 0); err != nil {
			return nil, fmt.Errorf("strict tool %s: %w", out[i].Name, err)
		}
		raw, err := json.Marshal(schema)
		if err != nil {
			return nil, errStrictSchema
		}
		out[i].Schema = raw
	}
	return out, nil
}

func makeStrictSchema(schema, root map[string]any, depth int) error {
	if depth > 64 {
		return errStrictSchema
	}
	// These constructs cannot be translated by adding nullable omitted fields.
	// Refuse them explicitly instead of changing an action's accepted meaning.
	for _, key := range []string{"allOf", "oneOf", "not", "if", "then", "else", "dependentRequired", "dependentSchemas", "dependencies", "patternProperties", "unevaluatedProperties", "minProperties", "maxProperties", "$dynamicRef", "$id", "$anchor", "$dynamicAnchor"} {
		if _, ok := schema[key]; ok {
			return fmt.Errorf("%w: unsupported %s", errStrictSchema, key)
		}
	}
	if ref, ok := schema["$ref"].(string); ok {
		if _, _, err := strictSchemaReference(root, ref); err != nil {
			return err
		}
	}
	for _, key := range []string{"properties", "$defs", "definitions"} {
		if values, ok := schema[key].(map[string]any); ok {
			for _, value := range values {
				child, ok := value.(map[string]any)
				if !ok {
					return errStrictSchema
				}
				if err := makeStrictSchema(child, root, depth+1); err != nil {
					return err
				}
			}
		}
	}
	if child, ok := schema["items"].(map[string]any); ok {
		if err := makeStrictSchema(child, root, depth+1); err != nil {
			return err
		}
	} else if _, exists := schema["items"]; exists {
		return errStrictSchema
	}
	for _, key := range []string{"anyOf", "prefixItems"} {
		if values, ok := schema[key].([]any); ok {
			for _, value := range values {
				child, ok := value.(map[string]any)
				if !ok {
					return errStrictSchema
				}
				if err := makeStrictSchema(child, root, depth+1); err != nil {
					return err
				}
			}
		}
	}
	if schemaObject(schema) {
		if schema["additionalProperties"] != false {
			return fmt.Errorf("%w: object must reject unknown properties", errStrictSchema)
		}
		properties, _ := schema["properties"].(map[string]any)
		if properties == nil {
			properties = map[string]any{}
			schema["properties"] = properties
		}
		originalRequired := schemaRequired(schema)
		required := make([]string, 0, len(properties))
		for name, property := range properties {
			required = append(required, name)
			if !originalRequired[name] {
				// A separate branch also admits null for enums and references. Changing
				// only type would still leave their original constraints rejecting null.
				properties[name] = map[string]any{"anyOf": []any{property, map[string]any{"type": "null"}}}
			}
		}
		for name := range originalRequired {
			if _, ok := properties[name]; !ok {
				return errStrictSchema
			}
		}
		sort.Strings(required)
		schema["required"] = required
	}
	// Defaults are execution annotations, unsupported by some strict providers.
	delete(schema, "default")
	return nil
}

func schemaObject(schema map[string]any) bool {
	if _, ok := schema["properties"]; ok {
		return true
	}
	if schema["type"] == "object" {
		return true
	}
	if types, ok := schema["type"].([]any); ok {
		for _, value := range types {
			if value == "object" {
				return true
			}
		}
	}
	return false
}
func schemaRequired(schema map[string]any) map[string]bool {
	names := map[string]bool{}
	if required, ok := schema["required"].([]any); ok {
		for _, value := range required {
			if name, ok := value.(string); ok {
				names[name] = true
			}
		}
	}
	return names
}
func schemaPointerToken(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
func strictSchemaReference(root map[string]any, ref string) (map[string]any, string, error) {
	if ref != "#" && !strings.HasPrefix(ref, "#/") {
		return nil, "", fmt.Errorf("%w: references must be local JSON pointers", errStrictSchema)
	}
	path, err := url.PathUnescape(strings.TrimPrefix(ref, "#"))
	if err != nil {
		return nil, "", errStrictSchema
	}
	var value any = root
	if path != "" {
		for _, encoded := range strings.Split(path[1:], "/") {
			token := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
			switch parent := value.(type) {
			case map[string]any:
				value = parent[token]
			case []any:
				index, err := strconv.Atoi(token)
				if err != nil || index < 0 || index >= len(parent) {
					return nil, "", errStrictSchema
				}
				value = parent[index]
			default:
				return nil, "", errStrictSchema
			}
		}
	}
	schema, ok := value.(map[string]any)
	if !ok {
		return nil, "", errStrictSchema
	}
	return schema, path, nil
}

// NormalizeStrictToolArguments restores omission only for fields made nullable
// by strict conversion. Originally nullable values, required fields, unknown
// fields, zero/false/empty values and exact JSON numbers keep their meaning.
// Native tool calls stay untouched for provider continuation; use this copy only
// for execution and its durable operation record.
func NormalizeStrictToolArguments(schema, arguments json.RawMessage) (json.RawMessage, error) {
	var root map[string]any
	var original jsonschema.Schema
	if json.Unmarshal(schema, &root) != nil || root["type"] != "object" || json.Unmarshal(schema, &original) != nil {
		return nil, errStrictSchema
	}
	resolved, err := original.Resolve(nil)
	if err != nil {
		return nil, errStrictSchema
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	if !json.Valid(arguments) || decoder.Decode(&value) != nil {
		return nil, errProviderData
	}
	n := strictArgumentNormalizer{root: root, original: &original, validators: map[string]*jsonschema.Resolved{"": resolved}}
	value, err = n.normalize(root, "", value, 0)
	if err != nil {
		return nil, err
	}
	if !n.validAt("", value) {
		return nil, errors.New("tool arguments do not match its original schema")
	}
	return json.Marshal(value)
}

type strictArgumentNormalizer struct {
	root       map[string]any
	original   *jsonschema.Schema
	validators map[string]*jsonschema.Resolved
}

func (n *strictArgumentNormalizer) validAt(path string, value any) bool {
	resolved := n.validators[path]
	if resolved == nil {
		const document = "urn:forgetmenote:strict-tool"
		schema := &jsonschema.Schema{Ref: document + "#" + path}
		var err error
		resolved, err = schema.Resolve(&jsonschema.ResolveOptions{Loader: func(uri *url.URL) (*jsonschema.Schema, error) {
			if uri.String() != document {
				return nil, errStrictSchema
			}
			return n.original, nil
		}})
		if err != nil {
			return false
		}
		n.validators[path] = resolved
	}
	// Match the shared dispatcher's schema validation representation. Keep the
	// lossless json.Number tree separately for serialization: this validator
	// version identifies json.Number as a string during type checks.
	if value == nil {
		return resolved.Validate(nil) == nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var validationValue any
	if json.Unmarshal(raw, &validationValue) != nil {
		return false
	}
	return resolved.Validate(validationValue) == nil
}
func (n *strictArgumentNormalizer) normalize(schema map[string]any, path string, value any, depth int) (any, error) {
	if depth > 64 {
		return nil, errProviderLimit
	}
	if ref, ok := schema["$ref"].(string); ok {
		target, targetPath, err := strictSchemaReference(n.root, ref)
		if err != nil {
			return nil, err
		}
		value, err = n.normalize(target, targetPath, value, depth+1)
		if err != nil {
			return nil, err
		}
	}
	if branches, ok := schema["anyOf"].([]any); ok {
		matched := false
		var selected any
		for i, branch := range branches {
			child, ok := branch.(map[string]any)
			if !ok {
				return nil, errStrictSchema
			}
			childPath := path + "/anyOf/" + strconv.Itoa(i)
			candidate, err := n.normalize(child, childPath, value, depth+1)
			if err == nil && n.validAt(childPath, candidate) {
				if matched && !reflect.DeepEqual(selected, candidate) {
					return nil, errors.New("strict tool arguments have ambiguous optional fields")
				}
				selected, matched = candidate, true
			}
		}
		if !matched {
			return nil, errors.New("tool arguments do not match its original schema")
		}
		value = selected
	}
	switch current := value.(type) {
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		required := schemaRequired(schema)
		result := make(map[string]any, len(current))
		for name, value := range current {
			child, known := properties[name].(map[string]any)
			if known {
				childPath := path + "/properties/" + schemaPointerToken(name)
				if value == nil && !required[name] && !n.validAt(childPath, nil) {
					continue
				}
				var err error
				value, err = n.normalize(child, childPath, value, depth+1)
				if err != nil {
					return nil, err
				}
			}
			result[name] = value
		}
		return result, nil
	case []any:
		result := make([]any, len(current))
		for i, value := range current {
			child, _ := schema["items"].(map[string]any)
			childPath := path + "/items"
			if prefix, ok := schema["prefixItems"].([]any); ok && i < len(prefix) {
				child, _ = prefix[i].(map[string]any)
				childPath = path + "/prefixItems/" + strconv.Itoa(i)
			}
			if child != nil {
				var err error
				value, err = n.normalize(child, childPath, value, depth+1)
				if err != nil {
					return nil, err
				}
			}
			result[i] = value
		}
		return result, nil
	}
	return value, nil
}
