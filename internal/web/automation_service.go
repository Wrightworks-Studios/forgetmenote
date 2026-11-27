package web

import (
	"forgetmenote/internal/automationrun"
	"forgetmenote/internal/backups"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/messaginghttp"
	"forgetmenote/internal/personal"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/taskqueue"
)

// initAutomationRun starts the automation run engine with automationRunHooks.
// It runs right after the automation runtime opens, before task queues,
// messaging and the personal assistant start, which the engine reads at each
// call.
func (s *Server) initAutomationRun() error {
	var err error
	s.automationrun, err = automationrun.New(s.catalog, s.automations, s.chatrun, s.memory, s.outgoing, s.data, s.automationRunHooks())
	return err
}

// automationRunHooks binds the engine to this instance's checks: a run's
// identity, its recheck and the instance AI policy (automation.go), the
// recheck of a run's evidence (automation_evidence.go), client access
// (credentials.go), data connections (data_connections.go) and personal
// context and consent (personal_ui.go), and to the action dispatcher
// (actions.go). The chat runtime, data connections, repository sources,
// backups, messaging, the personal assistant, task queues and the Slack app
// are read at each call, since web sets or replaces them after startup or
// starts them after the engine.
func (s *Server) automationRunHooks() automationrun.Hooks {
	return automationrun.Hooks{
		Chat:            func() *chat.Runtime { return s.chat },
		DataConnections: func() *connectors.Manager { return s.dataConnections },
		Repositories:    func() *reposource.Manager { return s.repositories },
		Backups: func() *backups.Manager {
			if s.backups == nil {
				return nil
			}
			return s.backups.manager
		},
		Messaging:       func() *messaginghttp.Service { return s.messaging },
		Personal:        func() *personal.Service { return s.personal },
		Tasks:           func() *taskqueue.Service { return s.tasks },
		SlackConfigured: func() bool { return s.slack != nil },

		RunContext:    s.automationContext,
		CheckRun:      s.checkAutomationAuth,
		Policy:        s.automationPolicy,
		AIWriting:     s.instanceAIWriting,
		CheckEvidence: s.checkAutomationEvidence,

		ClientAccess: s.clientAccess,
		DataExecute:  s.dataExecute,
		DataList:     s.dataList,

		DispatchAction:        s.dispatchActionOutcome,
		DocumentContentScope:  s.documentContentScope,
		ResolveSourceBindings: s.resolveChatSourceBindings,
		AcquireEngine:         s.acquireEngine,
		AcquireSourceWork:     s.acquireSourceWork,

		PersonalContext: s.personalChatContext,
		ConsentStamp:    s.personalConsentStamp,
		PersonalSpecs:   s.personalSpecs,
		EmailPairing:    s.runEmailPairing,

		RefreshPackages: s.refreshAutomationPackages,
		RunScript:       s.runScript,
		RunLearning:     s.runLearning,
		RunRepair:       s.runRepairChecks,
	}
}
