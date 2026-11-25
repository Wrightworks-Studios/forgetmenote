package webcore

import (
	"forgetmenote/internal/workspace"
	"net/http"
	"strings"
)

// NotebookRoutes owns method matching and its required catalog role together.
// It is assembled before request admission; a missing/invalid policy fails closed.
type NotebookRoutes struct {
	Mux              *http.ServeMux
	roles            map[string]string
	rejectedPaths    map[string]MethodRoles
	rejectedPrefixes map[string]MethodRoles
}
type MethodRoles struct{ Read, Write string }

func NewNotebookRoutes() *NotebookRoutes {
	return &NotebookRoutes{Mux: http.NewServeMux(), roles: map[string]string{}, rejectedPaths: map[string]MethodRoles{}, rejectedPrefixes: map[string]MethodRoles{}}
}
func validRole(role string) bool {
	return role == "read" || role == "comment" || role == "edit" || role == "owner"
}
func (routes *NotebookRoutes) Register(pattern, role string, handler http.HandlerFunc) {
	if !validRole(role) {
		panic("invalid notebook route role")
	}
	routes.Mux.HandleFunc(pattern, handler)
	routes.roles[pattern] = role
	method, path, _ := strings.Cut(pattern, " ")
	if method == "POST" && role != "edit" && !strings.Contains(path, "{") {
		routes.rejectedPaths[path] = MethodRoles{Write: role}
	}
}
func (routes *NotebookRoutes) RejectPath(path string, roles MethodRoles) {
	routes.validateRejected(roles)
	routes.rejectedPaths[path] = roles
}
func (routes *NotebookRoutes) RejectPrefix(prefix string, roles MethodRoles) {
	routes.validateRejected(roles)
	routes.rejectedPrefixes[prefix] = roles
}
func (routes *NotebookRoutes) validateRejected(roles MethodRoles) {
	if roles.Read != "" && !validRole(roles.Read) || roles.Write != "" && !validRole(roles.Write) {
		panic("invalid rejected notebook route role")
	}
}
func (routes *NotebookRoutes) Match(request *http.Request) (pattern, role string, err error) {
	_, pattern = routes.Mux.Handler(request)
	if pattern != "" {
		role = routes.roles[pattern]
		if !validRole(role) {
			return pattern, "", workspace.ErrForbidden
		}
		return pattern, role, nil
	}
	roles := routes.rejectedPaths[request.URL.Path]
	longest := 0
	for prefix, candidate := range routes.rejectedPrefixes {
		if len(prefix) > longest && strings.HasPrefix(request.URL.Path, prefix) {
			roles, longest = candidate, len(prefix)
		}
	}
	if request.Method == "GET" || request.Method == "HEAD" {
		role = roles.Read
		if role == "" {
			role = "read"
		}
	} else {
		role = roles.Write
		if role == "" {
			role = "edit"
		}
	}
	return "", role, nil
}
