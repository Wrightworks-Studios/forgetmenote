package connectors

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// This adapter calls a curated Python registry through the existing read-only
// broker. Module/function names are operator policy; callers supply JSON only.
type BrokerConfig struct {
	Module          string `json:"module"`
	CatalogFunction string `json:"catalogFunction"`
	InvokeFunction  string `json:"invokeFunction"`
	JSONEncoder     string `json:"jsonEncoder,omitempty"`
}

var pythonName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func pythonModule(name string) bool {
	for _, part := range strings.Split(name, ".") {
		if !pythonName.MatchString(part) {
			return false
		}
	}
	return name != ""
}
func validateBroker(c Connection) error {
	b := c.Broker
	if b == nil || !pythonModule(b.Module) || !pythonName.MatchString(b.CatalogFunction) || !pythonName.MatchString(b.InvokeFunction) || b.JSONEncoder != "" && (!pythonModule(b.JSONEncoder) || !strings.Contains(b.JSONEncoder, ".")) {
		return errors.New("broker requires a curated registry module and functions")
	}
	for _, op := range c.Operations {
		if !op.ReadOnly {
			return errors.New("broker helpers must be read-only")
		}
	}
	return nil
}
func (r *runtime) brokerRun(ctx context.Context, name string, input map[string]any, secret string) (map[string]json.RawMessage, error) {
	b := r.config.Broker
	code := "import json as _j\nfrom " + b.Module + " import " + b.CatalogFunction + " as _list, " + b.InvokeFunction + " as _call\n"
	encoder := ""
	if b.JSONEncoder != "" {
		last := strings.LastIndex(b.JSONEncoder, ".")
		module := b.JSONEncoder[:last]
		code += "from " + module + " import " + b.JSONEncoder[last+1:] + " as _encoder\n"
		encoder = ",cls=_encoder"
	}
	if name == "" {
		code += "_result={'tools':_list()}\n"
	} else {
		raw, _ := json.Marshal(map[string]any{"name": name, "arguments": input})
		code += "import base64 as _b\n_request=_j.loads(_b.b64decode('" + base64.StdEncoding.EncodeToString(raw) + "'))\n_result={'tool':_request['name'],'result':_call(_request['name'],_request['arguments'])}\n"
	}
	code += "print('__FMN_HELPER_JSON__='+_j.dumps(_result,separators=(',',':')" + encoder + "))\n"
	if len(code) > 16384 {
		return nil, errors.New("helper arguments exceed broker limit")
	}
	body, _ := json.Marshal(map[string]any{"reason": "ForgetMeNote curated helper " + name, "code": code, "timeoutSeconds": max(1, r.config.TimeoutSeconds-5)})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(r.config.Endpoint, "/")+"/v1/shell-ro/run", bytes.NewReader(body))
	if err != nil {
		return nil, ErrInvalid
	}
	req.Header.Set("Content-Type", "application/json")
	r.authorize(req, secret)
	if err := checkAuthority(ctx); err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, ErrUnavailable
	}
	raw, err := readBounded(resp.Body, r.config.MaxBytes)
	if err != nil {
		return nil, ErrUnavailable
	}
	var result struct {
		ExitCode        *int   `json:"exitCode"`
		Stdout          string `json:"stdout"`
		TimedOut        bool   `json:"timedOut"`
		StdoutTruncated bool   `json:"stdoutTruncated"`
		StderrTruncated bool   `json:"stderrTruncated"`
	}
	if json.Unmarshal(raw, &result) != nil || result.ExitCode == nil || *result.ExitCode != 0 || result.TimedOut || result.StdoutTruncated || result.StderrTruncated {
		return nil, errors.New("helper failed or returned incomplete output")
	}
	var envelope map[string]json.RawMessage
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.HasPrefix(line, "__FMN_HELPER_JSON__=") {
			if envelope != nil || json.Unmarshal([]byte(strings.TrimPrefix(line, "__FMN_HELPER_JSON__=")), &envelope) != nil {
				return nil, ErrUnavailable
			}
		}
	}
	if envelope == nil {
		return nil, ErrUnavailable
	}
	return envelope, nil
}
func (r *runtime) brokerCall(ctx context.Context, name string, input map[string]any, secret string) (any, error) {
	v, err := r.brokerRun(ctx, name, input, secret)
	if err != nil {
		return nil, err
	}
	var tool string
	json.Unmarshal(v["tool"], &tool)
	var result any
	if tool != name || json.Unmarshal(v["result"], &result) != nil {
		return nil, ErrUnavailable
	}
	return result, nil
}
func (r *runtime) brokerTools(ctx context.Context, secret string) ([]Operation, error) {
	v, err := r.brokerRun(ctx, "", nil, secret)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Name, Description string
		Arguments         map[string]map[string]any
	}
	if json.Unmarshal(v["tools"], &entries) != nil || len(entries) > 4096 {
		return nil, ErrUnavailable
	}
	out := []Operation{}
	seen := map[string]bool{}
	for _, op := range r.config.Operations {
		for _, entry := range entries {
			if op.Name != entry.Name {
				continue
			}
			if seen[op.Name] {
				return nil, ErrInvalid
			}
			seen[op.Name] = true
			op.InputSchema, err = brokerSchema(entry.Arguments)
			if err != nil {
				return nil, err
			}
			if op.Description == "" {
				op.Description = entry.Description
			}
			out = append(out, op)
		}
	}
	return out, nil
}
func brokerSchema(arguments map[string]map[string]any) (json.RawMessage, error) {
	properties := map[string]any{}
	required := []string{}
	groups := map[string][]string{}
	for name, spec := range arguments {
		property, err := brokerProperty(spec)
		if err != nil {
			return nil, err
		}
		properties[name] = property
		if spec["required"] == true {
			required = append(required, name)
		}
		var group []string
		if values, ok := spec["required_one_of"].([]any); ok {
			for _, v := range values {
				s, ok := v.(string)
				if !ok {
					return nil, ErrInvalid
				}
				group = append(group, s)
			}
		} else if other, ok := spec["required_with"].(string); ok {
			group = []string{name, other}
		}
		if len(group) > 0 {
			sort.Strings(group)
			groups[strings.Join(group, "\x00")] = group
		}
	}
	sort.Strings(required)
	schema := map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	all := []any{}
	keys := []string{}
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		choices := []any{}
		for _, name := range groups[key] {
			if properties[name] == nil {
				return nil, ErrInvalid
			}
			choices = append(choices, map[string]any{"required": []string{name}})
		}
		all = append(all, map[string]any{"oneOf": choices})
	}
	if len(all) > 0 {
		schema["allOf"] = all
	}
	raw, _ := json.Marshal(schema)
	if _, err := resolveSchema(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func brokerProperty(spec map[string]any) (map[string]any, error) {
	property := map[string]any{"type": "string"}
	switch spec["type"] {
	case "integer", "boolean":
		property["type"] = spec["type"]
	case "array":
		items, ok := spec["items"].(map[string]any)
		if !ok {
			return nil, errors.New("helper array requires an item contract")
		}
		item, err := brokerProperty(items)
		if err != nil {
			return nil, err
		}
		property["type"], property["items"] = "array", item
	case "date":
		property["pattern"] = `^\d{4}-\d{2}-\d{2}$`
	case "datetime":
		property["pattern"] = `^\d{4}-\d{2}-\d{2}T.+$`
	case "string", nil:
	default:
		return nil, errors.New("unsupported helper argument type")
	}
	for _, key := range []string{"minimum", "maximum", "default", "enum", "description"} {
		if v, ok := spec[key]; ok {
			property[key] = v
		}
	}
	if v, ok := spec["min_length"]; ok {
		property["minLength"] = v
	}
	if aliases, ok := spec["aliases"].(map[string]any); ok {
		if values, ok := property["enum"].([]any); ok {
			keys := []string{}
			for key := range aliases {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				values = append(values, key)
			}
			property["enum"] = values
		}
	}
	return property, nil
}
