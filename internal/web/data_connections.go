package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/dataops"
	"forgetmenote/internal/jsonfile"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) ConfigureDataConnections(file string) error {
	if err := s.privateRuntimeFile(file); err != nil {
		return err
	}
	var cfg connectors.Config
	if err := jsonfile.Load(file, 2<<20, &cfg); err != nil {
		return err
	}
	return s.ConfigureDataConnectionsConfig(cfg)
}
func (s *Server) ConfigureDataConnectionsConfig(cfg connectors.Config) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()

	manager, err := connectors.New(cfg)
	if err != nil {
		return err
	}
	for _, file := range manager.RuntimeFiles() {
		if err = s.privateRuntimeFile(file); err != nil {
			manager.Close()
			return err
		}
	}
	if s.dataConnections != nil {
		manager.Close()
		return errors.New("data connections are configured only at startup")
	}
	s.dataConnections = manager
	return nil
}
func (s *Server) dataAllowed(ctx context.Context, id, operation string) bool {
	if a, ok := automation.IdentityFrom(ctx); ok {
		if !slices.Contains(a.Spec.Connections, id) {
			return false
		}
		if a.Spec.Kind == "assistant" && a.Spec.Delivery != nil && a.Spec.Delivery.Shared {
			_, binding, _, err := s.messageBinding(*a.Spec.Delivery)
			if err != nil {
				return false
			}
			for _, actor := range binding.Actors {
				if !s.catalog.ConnectionAllowed(actor, id, operation) {
					return false
				}
			}
		}
		if a.Spec.Operations != nil {
			ops := a.Spec.Operations[id]
			if len(ops) == 0 || operation != "" && !slices.Contains(ops, operation) {
				return false
			}
		}
	}
	if s.checkCredential(ctx) != nil || !s.catalog.ConnectionAllowed(subject(ctx), id, operation) {
		return false
	}
	if grant, ok := credentialFrom(ctx); ok && !slices.Contains(grant.Grant.Connections, id) {
		return false
	}
	info, ok := s.dataConnections.Info(id)
	if !ok {
		return false
	}
	for _, project := range s.dataConnections.Projects(id) {
		if _, err := s.clientAccess(ctx, subject(ctx), project, "read"); err != nil {
			return false
		}
	}
	a, automated := automation.IdentityFrom(ctx)
	if info.CodeOnly && !(automated && a.Spec.Kind == "script" && a.Spec.Script != nil) {
		return false
	}
	if !info.ExternalProcessing {
		trustedScript, _ := ctx.Value(scriptHostContextKey{}).(bool)
		if chat.RunScopeFrom(ctx) != nil && !(info.CodeOnly && automated && a.Spec.Kind == "script" && a.Spec.Script != nil && trustedScript) {
			return false
		}
		if automated && a.Spec.UsesAI() && !(a.Spec.Script != nil && info.CodeOnly) {
			return false
		}
	}
	return true
}
func (s *Server) dataList(ctx context.Context) []connectors.Info {
	out := []connectors.Info{}
	if !s.hasScope(ctx, "data:read") {
		return out
	}
	for _, info := range s.dataConnections.List() {
		if s.dataAllowed(ctx, info.ID, "") {
			out = append(out, info)
		}
	}
	return out
}

func (s *Server) checkDataCitations(ctx context.Context, citations []chat.Citation) error {
	for _, c := range citations {
		if c.Kind == "data" && !s.dataAllowed(ctx, c.Source, c.References) {
			return workspace.ErrForbidden
		}
	}
	return nil
}
func (s *Server) checkDataConversation(ctx context.Context, c chat.Conversation) error {
	for _, r := range c.Runs {
		if e := s.checkDataCitations(ctx, r.Citations); e != nil {
			return e
		}
	}
	for _, t := range c.Turns {
		if e := s.checkDataCitations(ctx, t.Citations); e != nil {
			return e
		}
	}
	return nil
}
func (s *Server) dataTools(ctx context.Context, id string) ([]connectors.Operation, error) {
	if !s.hasScope(ctx, "data:read") || !s.dataAllowed(ctx, id, "") {
		return nil, workspace.ErrForbidden
	}
	ctx = connectors.WithAuthority(ctx, func(ctx context.Context) error {
		if !s.dataAllowed(ctx, id, "") {
			return workspace.ErrForbidden
		}
		return nil
	})
	ops, err := s.dataConnections.Tools(ctx, id)
	if err != nil {
		return nil, err
	}
	// A chat run never executes a write; it proposes one for its requester to
	// confirm, so it lists the writes its grants allow.
	a, automated := automation.IdentityFrom(ctx)
	proposes := automated && proposesWrites(a.Spec)
	out := []connectors.Operation{}
	for _, op := range ops {
		if s.dataAllowed(ctx, id, op.Name) && (op.ReadOnly || s.hasScope(ctx, "data:write") || proposes) {
			out = append(out, op)
		}
	}
	return out, nil
}

type dataCall = dataops.Call
type dataResult = dataops.Result

func (s *Server) dataExecute(ctx context.Context, in dataCall) (dataResult, error) {
	return s.dataOperations.Execute(ctx, in)
}

func (s *Server) initDataOperations() error {
	var err error
	s.dataOperations, err = dataops.New(s.catalog, dataops.Hooks{
		Manager: func() *connectors.Manager { return s.dataConnections },
		Allowed: s.dataAllowed,
		WriteAllowed: func(ctx context.Context, in dataCall) bool {
			return s.hasScope(ctx, "data:write") && chat.RunScopeFrom(ctx) == nil || confirmedDataWrite(ctx, in)
		},
		ReadProfile: func(ctx context.Context, in dataCall) error {
			store, err := s.clientAccess(ctx, subject(ctx), in.Project, "read")
			if err != nil {
				return err
			}
			_, err = readChatNote(ctx, store, in.ProfilePath, in.ProfileRevision)
			return err
		},
		Submit: s.dataSubmit,
		Observe: func(ctx context.Context, in dataCall, receipt connectors.Receipt, callErr error, started time.Time) {
			s.observeRecipeUsage(ctx, in, receipt, callErr, time.Since(started))
			s.observeLearningUsage(ctx, in, receipt, callErr, time.Since(started))
		},
		Outgoing: s.outgoing,
	})
	return err
}

// Routing is exact and deterministic. Only operator-configured maintenance can
// write; callers cannot choose its actor, checks, provider or acceptance criteria.
func (s *Server) observeRecipeUsage(ctx context.Context, in dataCall, receipt connectors.Receipt, callErr error, elapsed time.Duration) {
	if _, automated := automation.IdentityFrom(ctx); s.automations == nil || in.ProfilePath == "" || automated {
		return
	}
	for _, spec := range s.automations.Definitions.Specs() {
		if spec.Disabled || spec.Usage == nil || spec.Project != in.Project || spec.Recipe != in.ProfilePath {
			continue
		}
		matched := false
		for _, check := range spec.Checks {
			if check.Call.Connection == in.Connection && check.Call.Operation == in.Operation {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		outcome := "slow"
		if callErr != nil {
			if !spec.Usage.Errors || !errors.Is(callErr, connectors.ErrQuery) {
				continue
			}
			outcome = "query-error"
		} else if spec.Usage.SlowMS == 0 || elapsed.Milliseconds() < spec.Usage.SlowMS {
			continue
		}
		_, err := s.automations.Queue.Observe(spec, automation.Observation{ProfileRevision: in.ProfileRevision, Connection: in.Connection, Operation: in.Operation, Outcome: outcome, DurationMS: elapsed.Milliseconds(), Receipt: receipt.ID})
		state := "queued"
		if err != nil {
			state = "failed"
		}
		_ = s.catalog.RecordActivity(subject(ctx), "recipe.maintenance", in.Project, state, map[string]string{"job": spec.ID})
	}
}
func (s *Server) dataConnectionTools(server *mcp.Server) {
	s.dataEffectTools(server)
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	addAction(s, server, &mcp.Tool{Name: "list_data_connections", Description: "List live data connections this identity can use. Credentials and endpoints are not returned.", Annotations: readOnly}, func(ctx context.Context, in struct {
		Project string `json:"project,omitempty"`
	}) ([]connectors.Info, error) {
		return s.dataList(ctx), nil
	}, "data:read")
	addAction(s, server, &mcp.Tool{Name: "list_data_operations", Description: "Inspect the permitted operations and their exact input schemas for a connection. Profiles are instructions; choose and call an operation explicitly.", Annotations: readOnly}, func(ctx context.Context, in struct {
		Project    string `json:"project,omitempty"`
		Connection string `json:"connection"`
	}) ([]connectors.Operation, error) {
		v, e := s.dataTools(ctx, in.Connection)
		return v, e
	}, "data:read")
	addAction(s, server, &mcp.Tool{Name: "call_data_operation", Description: "Call one permitted operation with schema-valid arguments. Read a relevant recipe first when useful. Optional learningContext correlates a task and its intent for configured usage learning; it does not grant permissions. The receipt identifies the live source, time, and completeness; execution alone is not semantic verification."}, func(ctx context.Context, in dataCall) (dataResult, error) {
		v, e := s.dataExecute(ctx, in)
		return v, e
	}, "data:read")
	addManagementTool(s, server, "data_connection_access", "Inspect connection grants as an instance administrator.", true, func(ctx context.Context, in struct {
		Connection string `json:"connection"`
	}) (catalog.ConnectionGrant, error) {
		return s.catalog.ConnectionAccess(subject(ctx), in.Connection)
	})
	addManagementTool(s, server, "set_data_connection_access", "Replace explicit connection operation grants with revision checking as an instance administrator.", false, func(ctx context.Context, in catalog.ConnectionGrant) (catalog.ConnectionGrant, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return catalog.ConnectionGrant{}, workspace.ErrForbidden
		}
		if _, ok := s.dataConnections.Info(in.Connection); !ok {
			return catalog.ConnectionGrant{}, errors.New("connection is not configured")
		}
		for _, grants := range []map[string][]string{in.Users, in.Groups} {
			for _, names := range grants {
				for _, name := range names {
					if _, ok := s.dataConnections.ConfiguredOperation(in.Connection, name); !ok {
						return catalog.ConnectionGrant{}, errors.New("operation is not configured")
					}
				}
			}
		}
		return s.catalog.SetConnectionAccess(subject(ctx), in)
	})
	addManagementTool(s, server, "bind_external_identity", "Bind an external issuer/subject to an existing account as instance administrator. Never use an email match as proof of identity.", false, func(ctx context.Context, in struct {
		Issuer  string `json:"issuer"`
		Subject string `json:"subject"`
		User    string `json:"user"`
	}) (actionResult, error) {
		err := s.catalog.BindExternalIdentity(subject(ctx), in.Issuer, in.Subject, in.User)
		return actionResult{err == nil}, err
	})
}

// Browser requests reach the same typed handlers as API and MCP, with the
// ordinary session and CSRF checks applied by ServeHTTP.
func (s *Server) dataConnectionRoutes() {
	s.mux.HandleFunc("POST /integrations/actions/{action}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("action")
		if !strings.HasPrefix(name, "list_data_") && name != "call_data_operation" && name != "data_connection_access" && name != "set_data_connection_access" && name != "bind_external_identity" {
			http.NotFound(w, r)
			return
		}
		r.URL.Path = apiPrefix + "/actions/" + name
		s.apiHTTP(w, r)
	})
}
