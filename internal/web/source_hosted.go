package web

import (
	"context"
	"errors"
	"path"
	"slices"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/workspace"
)

func (s *Server) sourceProviderAvailability() (vision, transcription bool) {
	snapshot := s.chat.Snapshot()
	cfg := snapshot.Config
	if cfg == nil || cfg.Sources == nil || cfg.LocalOnly || snapshot.Closed {
		return false, false
	}
	return cfg.Sources.Model != "", cfg.Sources.TranscriptionModel != ""
}

// The same check runs at admission, before each upload and after each response.
// It is independent of chat runs and never treats a configured key as consent.
func (s *Server) sourceProcessingPolicy(ctx context.Context, project string, input sources.JobInput, expected *chat.Config) (*chat.Config, chat.Connection, error) {
	denied := workspace.ErrForbidden
	if err := ctx.Err(); err != nil {
		return nil, chat.Connection{}, err
	}
	// Admission, each queued job and each provider call: hosted extraction is
	// paid background work, so a paused copy runs none, even one copied queued.
	if err := s.outgoing.Check(); err != nil {
		return nil, chat.Connection{}, err
	}
	store, err := s.toolStore(ctx, project, "edit")
	if err != nil {
		return nil, chat.Connection{}, err
	}
	policy, managed := s.catalog.AISettings()
	if !managed || input.Options.ExternalOCR && !policy.SourceOCR || input.Options.DescribeImages && !policy.SourceDescriptions || input.Options.Transcribe && !policy.SourceTranscription {
		return nil, chat.Connection{}, denied
	}
	snapshot := s.chat.Snapshot()
	cfg, closed := snapshot.Config, snapshot.Closed
	if cfg == nil || cfg.Sources == nil || cfg.LocalOnly || closed || expected != nil && expected != cfg {
		return nil, chat.Connection{}, denied
	}
	var connection chat.Connection
	for _, candidate := range cfg.Connections {
		if candidate.ID == cfg.Sources.Connection {
			connection = candidate
		}
	}
	if connection.ID == "" || input.Options.ExternalOCR && cfg.Sources.Model == "" || input.Options.DescribeImages && cfg.Sources.Model == "" || input.Options.Transcribe && cfg.Sources.TranscriptionModel == "" {
		return nil, connection, denied
	}
	if grant, ok := credentialFrom(ctx); ok && !slices.Contains(grant.Grant.Connections, connection.ID) {
		return nil, connection, denied
	}
	if auth, ok := ctx.Value(automationKey{}).(automationAuth); ok && !slices.Contains(auth.Spec.Connections, connection.ID) {
		return nil, connection, denied
	}
	for _, notebook := range s.catalog.List(subject(ctx)) {
		if notebook.ID == project && notebook.ProtectedInbox != "" {
			if s.quicknotes == nil {
				return nil, connection, denied
			}
			settings, err := s.quicknotes.Settings(subject(ctx))
			if err != nil || !settings.CloudConsent {
				return nil, connection, denied
			}
		}
	}
	destination := "sources"
	if input.Folder != nil {
		destination = *input.Folder
	}
	candidate := input.Path
	if candidate == "" {
		candidate = path.Join(destination, "source.md")
	}
	allowed, err := store.VisibleHistoryPaths(ctx, []string{candidate})
	if err != nil {
		return nil, connection, err
	}
	if !allowed[candidate] {
		return nil, connection, denied
	}
	if input.Path != "" {
		allowed, err := store.VisibleHistoryPaths(ctx, []string{input.Path})
		if err != nil {
			return nil, connection, err
		}
		if !allowed[input.Path] {
			return nil, connection, denied
		}
		// Refresh targets must still be source captures. Notebook visibility may
		// change independently of the authority generation while a job is running.
		document, err := store.Read(ctx, input.Path, "")
		if err != nil {
			return nil, connection, err
		}
		metadata, err := workspace.ParseMetadata(document.Content)
		if err != nil {
			return nil, connection, err
		}
		if metadata.Source == nil {
			return nil, connection, denied
		}
	}
	return cfg, connection, nil
}
func (s *Server) hostedSource(ctx context.Context, scope sources.JobScope, input sources.JobInput, id string, before func() error) (*sources.Hosted, error) {
	ctx, err := s.sourceContext(ctx, scope)
	if err != nil {
		return nil, err
	}
	cfg, connection, err := s.sourceProcessingPolicy(ctx, scope.Notebook, input, nil)
	if err != nil {
		return nil, err
	}
	ledger := s.chat.Snapshot().Ledger
	if ledger == nil {
		return nil, errors.New("hosted source paid-work ledger is unavailable")
	}
	authorize := func(callCtx context.Context) error {
		callCtx, err := s.sourceContext(callCtx, scope)
		if err != nil {
			return err
		}
		_, _, err = s.sourceProcessingPolicy(callCtx, scope.Notebook, input, cfg)
		return err
	}
	return sources.NewHosted(sources.HostedConfig{Endpoint: connection.BaseURL, Model: cfg.Sources.Model, TranscriptionModel: cfg.Sources.TranscriptionModel, SecretFile: connection.SecretFile, Local: connection.Local, ContextTokens: connection.Limits.ContextTokens, OutputTokens: connection.Limits.OutputTokens}, ledger, decision.Scope{Actor: scope.Subject, Notebook: scope.Notebook, Feature: "extraction", Run: id}, authorize, before)
}
