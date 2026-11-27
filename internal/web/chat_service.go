package web

import (
	"encoding/json"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/citationchecks"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/reposource"
)

// initChatRun starts the chat run engine with chatRunHooks. It runs right
// after the chat runtime opens, before messaging binds the engine's source
// checks.
func (s *Server) initChatRun() error {
	var err error
	s.chatrun, err = chatrun.New(s.catalog, s.db, s.chatRunHooks())
	return err
}

// chatRunHooks binds the engine to this instance's checks: the request's
// identity, the run's session and its recheck (chat_run_auth.go), client
// access and notebook roles (credentials.go, projects.go), data and source
// grants, the judgments authorization and personal consent, and to the action
// dispatcher with its chat guard (actions.go, chat_action_scope.go). The
// runtime, origin, data connections, repository sources and citation checks
// are read at each call, since web sets or replaces them after startup.
func (s *Server) chatRunHooks() chatrun.Hooks {
	return chatrun.Hooks{
		Chat:            func() *chat.Runtime { return s.chat },
		Origin:          func() string { return s.origin },
		DataConnections: func() *connectors.Manager { return s.dataConnections },
		Repositories:    func() *reposource.Manager { return s.repositories },

		CheckRun:                  s.checkRun,
		ClientAccess:              s.clientAccess,
		ToolStore:                 s.toolStore,
		CheckDataCitations:        s.checkDataCitations,
		CheckSourceScope:          s.checkSourceChatScope,
		JudgmentsConfigured:       s.judgmentsConfigured,
		JudgmentsAuthorized:       s.judgmentsAuthorized,
		PersonalConsentStamp:      s.personalConsentStamp,
		PersonalContext:           s.personalChatContext,
		RecordPersonalInteraction: s.recordPersonalInteraction,

		ActionTool: s.actionTool,
		ChatTools:  s.chatTools,

		AcquireEngine:       s.acquireEngine,
		InstructionsAt:      s.instructionsAt,
		RetrieveScoped:      s.retrieveScoped,
		DecisionPreferences: s.decisionPreferences,
		AcquireSourceWork:   s.acquireSourceWork,

		Interactive:           s.interactiveIdentity,
		Authenticated:         s.authenticated,
		RunAuth:               s.chatRunAuth,
		RunContext:            s.runContext,
		CheckDataConversation: s.checkDataConversation,
		ResolveCorpus:         s.resolveChatCorpus,
		ActionCatalog:         s.actionCatalog,
		Citations:             func() *citationchecks.Runtime { return s.citations },

		ActionArguments: func(project, name string, args json.RawMessage) (json.RawMessage, error) {
			_, canonical, err := s.actionArguments(project, name, args)
			return canonical, err
		},
		GuardAction:      s.guardChatAction,
		PersonalApproval: s.chatPersonalApproval,
		ReadOnly:         chatReadOnly,
		DispatchAction:   s.dispatchActionOutcome,
	}
}
