package workspace

import (
	"encoding/json"
)

// ContainerPolicy is resolved by the gateway. Owners never guess missing choices.
type ContainerPolicy struct {
	Version  int    `json:"version"`
	Archives string `json:"archives"`
	Drafts   string `json:"drafts"`
}

func (p ContainerPolicy) Validate() error {
	valid := func(value string) bool { return value == "include" || value == "lower" || value == "exclude" }
	if p.Version != 1 || !valid(p.Archives) || !valid(p.Drafts) {
		return invalidInput("container policy requires version 1 and include, lower, or exclude for Archives and Drafts")
	}
	return nil
}

type ContentScope struct {
	NotesOnly  bool            `json:"notesOnly,omitempty"`
	Policy     ContainerPolicy `json:"policy"`
	Collection string          `json:"collection"`
}

func (s ContentScope) Validate() error {
	if err := s.Policy.Validate(); err != nil {
		return err
	}
	if !ValidCollection(s.Collection) {
		return invalidInput("unknown note collection")
	}
	return nil
}
func (s ContentScope) ID() string {
	if s.Validate() != nil {
		return ""
	}
	if s.Collection == "" {
		s.Collection = "all"
	}
	raw, _ := json.Marshal(s)
	return ContentHash(raw)
}

// Tier is -1 for ineligible/unknown, 0 for included and 1 for lower priority.
func (s ContentScope) Tier(m Metadata) int {
	if m.ContainerError != "" || !InCollection(&m, s.Collection) {
		return -1
	}
	choice := "include"
	switch m.Container {
	case "":
	case "archive":
		choice = s.Policy.Archives
	case "draft":
		choice = s.Policy.Drafts
	default:
		return -1
	}
	switch choice {
	case "include":
		return 0
	case "lower":
		return 1
	default:
		return -1
	}
}

type ScopedEntries struct {
	Revision   string  `json:"revision"`
	Visibility string  `json:"visibility"`
	ScopeID    string  `json:"scopeId"`
	Entries    []Entry `json:"entries"`
}

// NotesOnly is selected by integrated chat, never by model query arguments.
func (s ContentScope) AllowsPath(path string) bool {
	return !s.NotesOnly || path != "AGENTS.md" && !IsTrash(path)
}
