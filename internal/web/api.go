package web

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"forgetmenote/internal/webcore"
)

const apiPrefix = "/api/v1"

func apiError(w http.ResponseWriter, status int, code, message string) {
	webcore.WriteError(w, webcore.Error{Status: status, Code: code, Message: message}, true)
}
func apiFail(w http.ResponseWriter, err error) { webcore.FailAction(w, err) }

func (s *Server) apiHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && r.URL.Path == apiPrefix+"/actions" {
		release, err := admitRPC(r.Context(), s.rpcReadWork)
		if err != nil {
			apiFail(w, err)
			return
		}
		defer release()
		names := make([]string, 0, len(s.actions))
		for name := range s.actions {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]map[string]any, 0, len(names))
		for _, name := range names {
			a := s.actions[name]
			if !s.clientActionAvailable(r.Context(), name, a.ClientScope) {
				continue
			}
			out = append(out, map[string]any{"name": name, "description": a.Description, "scope": a.ClientScope, "inputSchema": a.Schema, "outputSchema": a.OutputSchema})
		}
		jsonResponse(w, out)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, apiPrefix+"/actions/")
	raw := []byte(`{}`)
	if r.Method == "GET" && r.URL.Path == apiPrefix+"/capabilities" {
		name = "capabilities"
	} else if r.Method != "POST" || !strings.HasPrefix(r.URL.Path, apiPrefix+"/actions/") {
		apiError(w, 404, "not_found", "API endpoint not found")
		return
	} else {
		var err error
		raw, err = io.ReadAll(r.Body)
		if err != nil {
			apiFail(w, err)
			return
		}
	}
	a, ok := s.actions[name]
	if !ok {
		apiError(w, 404, "not_found", "Action not found")
		return
	}
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil || value == nil || a.Resolved.Validate(value) != nil {
		apiError(w, 400, "invalid_request", "Arguments do not match the action schema")
		return
	}
	result, err := a.Call(r.Context(), raw)
	if err != nil {
		apiFail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}
