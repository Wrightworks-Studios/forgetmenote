package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) ConfigurePersonalConnections(file string) error {
	if err := s.privateRuntimeFile(file); err != nil {
		return err
	}
	cfg, err := connectors.LoadPersonalConfig(file)
	if err != nil {
		return err
	}
	return s.ConfigurePersonalConnectionsConfig(cfg)
}
func (s *Server) ConfigurePersonalConnectionsConfig(config connectors.PersonalConfig) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()

	if s.personalConnections != nil {
		return errors.New("personal connections are configured only at startup")
	}
	paths := []string{config.KeyFile}
	for _, app := range []*connectors.OAuthApplication{config.Google, config.Slack} {
		if app != nil {
			paths = append(paths, app.SecretFile)
		}
	}
	for _, path := range paths {
		if err = s.privateRuntimeFile(path); err != nil {
			return err
		}
	}
	s.personalConnections, err = connectors.OpenPersonalConnections(s.db, config)
	return err
}

// Every provider call, token refresh and new grant passes here first. A paused
// copy holds the live instance's grants; a refresh there can rotate the token
// the live instance still uses.
func (s *Server) personalAuthority(ctx context.Context, feature string) (catalog.Account, catalog.AssistantSettings, error) {
	if err := s.outgoing.Check(); err != nil {
		return catalog.Account{}, catalog.AssistantSettings{}, err
	}
	account, err := s.catalog.Account(subject(ctx))
	if err != nil {
		return account, catalog.AssistantSettings{}, err
	}
	settings, err := s.catalog.Assistant(subject(ctx))
	if err != nil {
		return account, settings, err
	}
	if s.personalConnections == nil || !s.catalog.AssistantPolicy().Enabled || !settings.Enabled || !settings.ExternalProcessing || feature != "" && !slices.Contains(settings.Features, feature) {
		return account, settings, workspace.ErrForbidden
	}
	if _, err = s.clientAccess(ctx, subject(ctx), settings.Project, "read"); err != nil {
		return account, settings, err
	}
	return account, settings, s.checkCredential(ctx)
}

// replyAuthority allows reading a person's Slack for their reply reminders:
// the instance offers personal assistants, personal connections are set up,
// and the person turned reply reminders on. Nothing it reads reaches a model,
// so it needs no AI processing consent; personalAuthority still guards every
// other personal read.
func (s *Server) replyAuthority(user string) (catalog.Account, error) {
	if err := s.outgoing.Check(); err != nil {
		return catalog.Account{}, err
	}
	account, err := s.catalog.Account(user)
	if err != nil {
		return account, err
	}
	settings, err := s.catalog.Assistant(user)
	if err != nil {
		return account, err
	}
	if s.personalConnections == nil || !s.catalog.AssistantPolicy().Enabled || !settings.ReplyReminders {
		return account, workspace.ErrForbidden
	}
	return account, nil
}

// personalConnectAuthority allows connecting a provider: for the personal
// assistant, or Slack alone for reply reminders.
func (s *Server) personalConnectAuthority(ctx context.Context, provider string) error {
	_, _, err := s.personalAuthority(ctx, "")
	if err == nil || provider != "slack" {
		return err
	}
	if _, e := s.replyAuthority(subject(ctx)); e != nil {
		return err
	}
	return s.checkCredential(ctx)
}

func (s *Server) personalConnectionRoutes() {
	for _, action := range []string{"connect", "callback", "disconnect"} {
		method := "POST"
		if action == "callback" {
			method = "GET"
		}
		s.mux.HandleFunc(method+" /account/connections/{provider}/"+action, func(w http.ResponseWriter, r *http.Request) {
			if api, _ := r.Context().Value(apiContextKey{}).(bool); api {
				fail(w, workspace.ErrForbidden)
				return
			}
			account, err := s.catalog.Account(subject(r.Context()))
			if err != nil {
				fail(w, err)
				return
			}
			if s.personalConnections == nil {
				http.Error(w, "Personal connections are not configured", 503)
				return
			}
			provider := r.PathValue("provider")
			if action == "disconnect" {
				if _, err = s.catalog.Assistant(subject(r.Context())); err == nil {
					err = s.personalConnections.Disconnect(account.ID, provider)
				}
				if err != nil {
					fail(w, err)
					return
				}
				http.Redirect(w, r, "/account/assistant", 303)
				return
			}
			if err = s.personalConnectAuthority(r.Context(), provider); err != nil {
				fail(w, err)
				return
			}
			path := "/account/connections/" + provider + "/callback"
			bound := connectionBinding{name: "fmn-personal-" + provider, path: path}
			epoch := s.catalog.Epoch(subject(r.Context()))
			if action == "connect" {
				value := RandomToken()
				target, err := s.personalConnections.Begin(account.ID, provider, value+":"+strconv.FormatUint(epoch, 10), s.origin+path)
				if err != nil {
					fail(w, err)
					return
				}
				if s.proxyIdentity(r) != "" {
					if err = s.createSession(w, subject(r.Context())); err != nil {
						fail(w, err)
						return
					}
				}
				s.setBinding(w, bound, value)
				s.allowCallback(w, target)
				http.Redirect(w, r, target, 303)
				return
			}
			value, err := s.binding(r, bound)
			s.clearBinding(w, bound)
			if err != nil || r.URL.Query().Get("error") != "" {
				fail(w, workspace.ErrForbidden)
				return
			}
			_, err = s.personalConnections.Finish(r.Context(), account.ID, provider, value+":"+strconv.FormatUint(epoch, 10), r.URL.Query().Get("state"), r.URL.Query().Get("code"), func() error {
				if !s.catalog.ValidSession(subject(r.Context()), epoch) {
					return workspace.ErrForbidden
				}
				return s.personalConnectAuthority(r.Context(), provider)
			})
			if err != nil {
				fail(w, err)
				return
			}
			http.Redirect(w, r, "/account/assistant", 303)
		})
	}
}

type personalSubmission struct {
	ActionID string                       `json:"actionID"`
	Call     connectors.PersonalOperation `json:"call"`
}

func (s *Server) personalSource(ctx context.Context, in connectors.PersonalOperation) (connectors.Result, error) {
	if !connectors.PersonalRead(in.Operation) || !s.hasScope(ctx, "data:read") {
		return connectors.Result{}, workspace.ErrForbidden
	}
	return s.personalCall(ctx, in)
}
func (s *Server) personalCall(ctx context.Context, in connectors.PersonalOperation) (connectors.Result, error) {
	if err := in.Validate(); err != nil {
		return connectors.Result{}, err
	}
	account, settings, err := s.personalAuthority(ctx, connectors.PersonalFeature(in.Operation))
	if err != nil {
		return connectors.Result{}, err
	}
	epoch := s.catalog.Epoch(subject(ctx))
	policy := s.catalog.AssistantPolicy().Revision
	return s.personalConnections.Call(ctx, account.ID, in, func(ctx context.Context) error {
		_, current, err := s.personalAuthority(ctx, connectors.PersonalFeature(in.Operation))
		if err != nil {
			return err
		}
		if current.Revision != settings.Revision || policy != s.catalog.AssistantPolicy().Revision || !s.catalog.ValidSession(subject(ctx), epoch) {
			return workspace.ErrForbidden
		}
		return nil
	})
}

// accountUIActions run only through accountUIRequest, so MCP and API
// discovery do not offer them.
var accountUIActions = map[string]bool{"submit_personal_action": true, "prepare_assistant_draft": true, "recover_assistant_work": true}

// accountUIRequest admits only the signed-in person's own browser request.
// OAuth and fmn_ grants, the recovery bearer, chat and background jobs never
// carry that approval, whatever scopes they hold.
func (s *Server) accountUIRequest(ctx context.Context) error {
	_, scoped := ctx.Value(scopeKey{}).(string)
	_, credential := credentialFrom(ctx)
	_, automated := ctx.Value(automationKey{}).(automationAuth)
	api, _ := ctx.Value(apiContextKey{}).(bool)
	if scoped || credential || automated || api || chatScopeFrom(ctx) != nil || attribution.From(ctx).Source == "mcp" || !s.catalog.Interactive(subject(ctx)) {
		return workspace.ErrForbidden
	}
	return nil
}

// Only the authenticated account page can authorize an external personal write.
// Models may propose it; their tool arguments are never user consent.
func (s *Server) personalSubmit(ctx context.Context, in personalSubmission) (connectors.Result, error) {
	if in.ActionID == "" || connectors.PersonalRead(in.Call.Operation) {
		return connectors.Result{}, connectors.ErrInvalid
	}
	if err := s.accountUIRequest(ctx); err != nil {
		return connectors.Result{}, err
	}
	return s.personalWrite(ctx, in)
}
func (s *Server) personalWrite(ctx context.Context, in personalSubmission) (connectors.Result, error) {
	account, _, err := s.personalAuthority(ctx, connectors.PersonalFeature(in.Call.Operation))
	if err != nil {
		return connectors.Result{}, err
	}
	if err = in.Call.Validate(); err != nil {
		return connectors.Result{}, err
	}
	if in.Call.Arguments == nil {
		in.Call.Arguments = map[string]any{}
	}
	in.Call.Arguments["actionID"] = in.ActionID
	e, created, err := s.automations.Queue.BeginEffect(automation.Effect{Scope: automation.Hash([]string{"personal", account.ID, in.Call.Provider}), Action: in.ActionID, InputHash: automation.Hash(in.Call), Actor: subject(ctx), Connection: "personal-" + in.Call.Provider, Operation: in.Call.Operation})
	if err != nil {
		return connectors.Result{}, err
	}
	if !created {
		var result connectors.Result
		err = json.Unmarshal(e.Result, &result)
		return result, err
	}
	result, callErr := s.personalCall(connectors.WithSubmission(ctx), in.Call)
	state := "accepted"
	if callErr != nil {
		state = "unknown"
		if connectors.WriteRejected(callErr) {
			state = "rejected"
		}
	}
	var saved any
	if callErr == nil {
		saved = result
	}
	if _, err = s.automations.Queue.FinishEffect(e.ID, state, saved); err != nil {
		return result, err
	}
	return result, callErr
}

func (s *Server) personalConnectionTools(server *mcp.Server) {
	addAction(s, server, &mcp.Tool{Name: "read_personal_source", Description: "Read your connected email, calendar, documents, spreadsheets or Slack conversations. The account and grant are implicit; provider/operation/generation must come from assistant_status. Cannot send or modify anything.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, in connectors.PersonalOperation) (connectors.Result, error) {
		out, err := s.personalSource(ctx, in)
		return out, err
	}, "data:read")
	addAction(s, server, &mcp.Tool{Name: "submit_personal_action", Description: "Account UI only: submit a personal action the employee explicitly approved on the account page. Always refused over MCP, API credentials, chat and background jobs."}, func(ctx context.Context, in personalSubmission) (connectors.Result, error) {
		out, err := s.personalSubmit(ctx, in)
		return out, err
	}, "data:write")
}
