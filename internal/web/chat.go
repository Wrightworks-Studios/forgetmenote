package web

import (
	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/webcore"
)

type chatService = chat.Runtime

func (s *Server) initChat() error {
	runtime, err := chat.OpenRuntime(s.db)
	if err != nil {
		return err
	}
	runtime.PrepareSource = sources.Prepare
	s.chat = runtime
	return nil
}
func (s *Server) ConfigureChat(path string) error {
	return s.ConfigureAI(path, "")
}

// ConfigureAI installs chat and decisions together so operator privacy policy and
// service replacement have one lock and one shared decision ledger. Either file may
// be absent; a standalone decision service does not enable chat.
func (s *Server) ConfigureAI(chatPath, decisionPath string) error {
	return s.chat.Configure(s.data, chatPath, decisionPath, s.chatConfigurationHooks())
}
func (s *Server) ConfigureAIConfig(cfg *chat.Config, judge *decision.Config) error {
	return s.chat.ConfigureValues(s.data, cfg, judge, s.chatConfigurationHooks())
}
func (s *Server) chatConfigurationHooks() chat.ConfigurationHooks {
	hooks := chat.ConfigurationHooks{Installed: func(previous, current *decision.Service, localOnly bool) error {
		if previous != nil && s.judgments != nil {
			if err := s.judgments.InvalidateOrganizationJobs(); err != nil {
				return err
			}
		}
		if s.quicknotes != nil {
			s.quicknotes.ConfigureJudgments(current, localOnly)
		}
		return s.refreshChatEmbeddingPolicy()
	}, WithoutJudgments: !s.modules.Enabled("judgments")}
	if s.judgments != nil {
		hooks.Guard = s.judgments.OrganizationGuard
	}
	return hooks
}
func (s *Server) chatRoutes() {
	// Chat lifecycle authorization historically precedes method/path rejection.
	s.routes.RejectPrefix("/api/chat/", webcore.MethodRoles{Write: "read"})
	s.chatrun.Register(s.mux, s.handleProject)
}
