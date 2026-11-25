package web

import (
	"context"
	"errors"
	"forgetmenote/internal/citationchecks"
	"forgetmenote/internal/decision"
	"os"
	"sync"
)

// This directory contains gateway orchestration metadata. Notebook content and
// shared findings/proposals remain in their existing Git/workspace owners.
type jevState struct {
	organizationMu sync.Mutex
	records        *decision.Records
	prepare        chan struct{}
	mu             sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	closed         bool
	active         map[string]context.CancelFunc
	wg             sync.WaitGroup
}
type jevPreferences = decision.Preferences
type jevUserSettings struct {
	OrganizationGeneration uint64         `json:"organizationGeneration,omitempty"`
	Version                int            `json:"version"`
	Owner                  string         `json:"owner"`
	Settings               jevPreferences `json:"settings"`
}

func (j *jevState) organizationConsentGeneration(user string) (uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var record jevUserSettings
	err := j.read("users/"+digest(user)+".json", &record)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return record.OrganizationGeneration, err
}

type jevNotebookSettings struct {
	Organization *organizationSettings `json:"organization,omitempty"`
	Baseline     map[string]string     `json:"baseline"`
	// BaselineRevision is the commit Baseline was read at, so an automatic
	// review can tell which passages of a changed note are new.
	BaselineRevision string            `json:"baselineRevision,omitempty"`
	Observed         map[string]string `json:"observed,omitempty"`
	ObservedAt       string            `json:"observedAt,omitempty"`
	Version          int               `json:"version"`
	Notebook         string            `json:"notebook"`
	AutoChecks       bool              `json:"autoChecks"`
	EnabledBy        string            `json:"enabledBy,omitempty"`
	AccountID        string            `json:"accountId,omitempty"`
}

func (s *Server) initJev() error {
	records, err := decision.OpenRecords(s.db)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.jevState = &jevState{records: records, ctx: ctx, cancel: cancel, active: map[string]context.CancelFunc{}, prepare: make(chan struct{}, 1)}
	s.citations = citationchecks.New(ctx, records)
	return nil
}

func (j *jevState) close() {
	if j == nil {
		return
	}
	j.mu.Lock()
	j.closed = true
	j.cancel()
	for _, cancel := range j.active {
		cancel()
	}
	j.mu.Unlock()
	j.wg.Wait()
}

func (j *jevState) read(name string, value any) error  { return j.records.Read(name, value) }
func (j *jevState) write(name string, value any) error { return j.records.Write(name, value) }

// update changes a record while holding it, so a change committed through
// another gateway is never overwritten (decision.Records.Update).
func (j *jevState) update(name string, value any, change func(found bool) error) error {
	return j.records.Update(name, value, change)
}

func (j *jevState) preferences(user string) (jevPreferences, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var record jevUserSettings
	err := j.read("users/"+digest(user)+".json", &record)
	if errors.Is(err, os.ErrNotExist) {
		return jevPreferences{Rerank: true, Citations: true}, nil
	}
	if err != nil || record.Version != 1 || record.Owner != user {
		return jevPreferences{}, decision.ErrStorage
	}
	return record.Settings, nil
}
