package web

import (
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/reposource"
)

// initChatRun starts the chat run engine with chatRunHooks. It runs right
// after the chat runtime opens, before messaging binds the engine's source
// checks.
func (s *Server) initChatRun() error {
	var err error
	s.chatrun, err = chatrun.New(s.catalog, s.chatRunHooks())
	return err
}

// chatRunHooks binds the engine to this instance's checks: the run recheck
// (chat_runner.go), client access and notebook roles (credentials.go,
// projects.go), data and source grants, Jev's authorization and personal
// consent. The runtime, origin, data connections and repository sources are
// read at each call, since web sets or replaces them after startup.
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
		JevConfigured:             s.jevConfigured,
		JevAuthorized:             s.jevAuthorized,
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

		ExecuteTool:        s.executeChatTool,
		PrepareOperation:   s.prepareChatOperation,
		StartCitationCheck: s.startCitationCheck,
	}
}
