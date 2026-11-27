package automationrun

import (
	"context"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
)

// Test-only exports: web's server-level tests reach these engine internals,
// and nothing else may. TestAutomationRunTestExportsStayInTests in web fails
// on a reference from production code.

// RunAutomation runs one run as the runtime does, so web's tests can drive a
// run, or the runtime's queue, directly.
func (s *Service) RunAutomation(ctx context.Context, r *automation.Run, save func() error) error {
	return s.runAutomation(ctx, r, save)
}

// A script's host state and its host calls, so web's tests can drive a
// script's host operations without running Python.
type ScriptExecution = scriptExecution
type ScriptRequest = scriptRequest

func NewScriptExecution(ctx context.Context, r *automation.Run, save func() error, info PackageInfo) *ScriptExecution {
	return &scriptExecution{ctx: ctx, run: r, save: save, info: info}
}
func (s *Service) RunScript(ctx context.Context, r *automation.Run, save func() error) error {
	return s.runScript(ctx, r, save)
}
func (s *Service) ScriptStep(e *ScriptExecution, in ScriptRequest) (any, error) {
	return s.scriptStep(e, in)
}
func (s *Service) ScriptCallAuthority(e *ScriptExecution, in ScriptRequest) error {
	return s.scriptCallAuthority(e, in)
}
func (s *Service) ScriptOperation(e *ScriptExecution, in ScriptRequest, started time.Time) (any, error) {
	return s.scriptOperation(e, in, started)
}
func (s *Service) ScriptDecisions(e *ScriptExecution, in ScriptRequest) (any, error) {
	return s.scriptDecisions(e, in)
}
func (s *Service) ScriptContract(spec automation.Spec) (string, error) { return s.scriptContract(spec) }
func (s *Service) AnalysisProvider(spec automation.Spec) map[string]string {
	return s.scriptAnalysisProvider(spec)
}

// SetScriptImageFetch and SetScriptWebsiteCapture replace a script's image
// fetch and website capture with a local fixture.
func (s *Service) SetScriptImageFetch(fetch func(context.Context, string) ([]byte, string, string, error)) {
	s.scriptImageFetch = fetch
}
func (s *Service) SetScriptWebsiteCapture(capture func(context.Context, string) (chat.WebsiteEvidence, error)) {
	s.scriptWebsiteCapture = capture
}

// Package updates, which the runtime otherwise starts on its schedule.
func (s *Service) RefreshPackages(ctx context.Context) { s.refreshAutomationPackages(ctx) }
func (s *Service) RefreshPackage(ctx context.Context, d automation.Definition) error {
	return s.refreshAutomationPackage(ctx, d)
}

// Learning's profile format and a repair's evidence.
type LearningCandidate = learningCandidate
type LearnedProfile = learnedProfile

func LearningExamples(events []automation.LearningEvent) []automation.LearningEvent {
	return learningExamples(events)
}
func LearningContent(old []byte, c LearningCandidate, record LearnedProfile) (string, error) {
	return learningContent(old, c, record)
}
func ProfileLearningRecord(content []byte) LearnedProfile { return profileLearningRecord(content) }
func (s *Service) RepairEvidence(ctx context.Context, r *automation.Run, results []automation.CheckResult) (string, []any, error) {
	return s.repairEvidence(ctx, r, results)
}
