package web

import (
	"errors"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/modules"
)

// wiredModule is what a module adds to the server beyond its hub: the MCP
// tool groups mcpHandler adds and the routes New registers. The server adds
// them only for a module it runs. An off module's actions are then absent
// from the registry that /api/v1, MCP, chat and automations all read, and its
// paths answer 404 like any unknown path. Its hub, workers and settings check
// s.modules where they start.
type wiredModule struct {
	name   string
	tools  []func(*Server, *mcp.Server)
	routes []func(*Server, routeRegistrar)
}

// routeRegistrar registers routes: a project route is a notebook route with
// the catalog role it requires, and a global route is not scoped to a
// notebook.
type routeRegistrar struct {
	project func(pattern, role string, handler http.HandlerFunc)
	global  func(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// wiredModules lists every module that can be turned off. A module whose
// wiring has not landed yet is not here, and internal/modules refuses to turn
// it off.
var wiredModules = []wiredModule{
	{name: "judgments", routes: []func(*Server, routeRegistrar){(*Server).judgmentRoutes}},
	{name: "data-connections", tools: []func(*Server, *mcp.Server){(*Server).dataConnectionTools}},
	{name: "sources", tools: []func(*Server, *mcp.Server){(*Server).sourceTools}, routes: []func(*Server, routeRegistrar){(*Server).sourceRoutes}},
	{name: "repositories", tools: []func(*Server, *mcp.Server){(*Server).repositoryTools}, routes: []func(*Server, routeRegistrar){(*Server).repositoryRoutes}},
	{name: "managed-backups", tools: []func(*Server, *mcp.Server){(*Server).backupTools}, routes: []func(*Server, routeRegistrar){(*Server).backupRoutes}},
	{name: "personal", tools: []func(*Server, *mcp.Server){(*Server).personalAssistantTools, (*Server).personalProfileTools, (*Server).personalConnectionTools, (*Server).personalUITools, (*Server).personalRecoveryTools}},
}

// gitHubConnections reports whether the instance runs the GitHub App
// connection flow that repository sources and managed backups share
// (connections.go). It runs while either module does; a connection for the
// other answers that it is not available.
func (s *Server) gitHubConnections() bool {
	return s.modules.Enabled("repositories") || s.modules.Enabled("managed-backups")
}

func (s *Server) moduleTools(server *mcp.Server) {
	for _, module := range wiredModules {
		if s.modules.Enabled(module.name) {
			for _, tools := range module.tools {
				tools(s, server)
			}
		}
	}
}

func (s *Server) moduleRoutes() {
	register := routeRegistrar{project: s.handleProject, global: s.mux.HandleFunc}
	for _, module := range wiredModules {
		if s.modules.Enabled(module.name) {
			for _, routes := range module.routes {
				routes(s, register)
			}
		}
	}
}

// ConfigureModules checks instance.json's modules.off (internal/modules)
// against the module set the server was built with (NewWithBlobs). An off
// module's routes, tools and workers are never added, so the set is fixed for
// the server's life, and changing it means restarting.
func (s *Server) ConfigureModules(off []string) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()
	set, err := modules.Parse(off)
	if err != nil {
		return err
	}
	if !set.Equal(s.modules) {
		return errors.New("modules.off differs from the module set this server was built with (" + s.modules.String() + "); restart the instance to change it")
	}
	return nil
}

// Modules is the module set the instance runs.
func (s *Server) Modules() modules.Set { return s.modules }

// modulesOff names the modules the instance turns off, for the browser to
// leave out their regions; it is empty, never nil, when every module runs.
func (s *Server) modulesOff() []string {
	off := []string{}
	for _, state := range s.modules.States() {
		if !state.Enabled {
			off = append(off, state.Name)
		}
	}
	return off
}
