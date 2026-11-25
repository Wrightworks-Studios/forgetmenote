package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/workspace"
	"io"
)

var ErrCorpus = errors.New("this run has no supported saved corpus; start a new turn with an explicit corpus selection")

// Corpus is the immutable, resolved selection for a run. Notebook defaults are
// not authority to change it later. Invalid saved scopes remain readable data.
type Corpus struct {
	NoteProjects   []string          `json:"noteProjects,omitempty"`
	SourceProjects []string          `json:"sourceProjects,omitempty"`
	SourceBindings map[string]string `json:"sourceBindings,omitempty"`
	Version        int               `json:"version"`
	Archives       string            `json:"archives"`
	Drafts         string            `json:"drafts"`
	History        bool              `json:"history"`
	invalid        json.RawMessage
}
type corpusWire struct {
	NoteProjects   []string          `json:"noteProjects,omitempty"`
	SourceProjects []string          `json:"sourceProjects,omitempty"`
	SourceBindings map[string]string `json:"sourceBindings,omitempty"`
	Version        int               `json:"version"`
	Archives       string            `json:"archives"`
	Drafts         string            `json:"drafts"`
	History        bool              `json:"history"`
}

func (c *Corpus) Validate() error {
	valid := func(v string) bool { return v == "include" || v == "lower" || v == "exclude" }
	if c == nil || len(c.invalid) > 0 || c.Version != 1 || !valid(c.Archives) || !valid(c.Drafts) {
		return ErrCorpus
	}

	raw, err := c.MarshalJSON()
	if err != nil || len(raw) > capacity.StateBytes.Limit() {
		return ErrCorpus
	}
	for _, projects := range [][]string{c.NoteProjects, c.SourceProjects} {
		seen := map[string]bool{}
		for _, p := range projects {
			if !workspace.ValidNotebookID(p) || seen[p] {
				return ErrCorpus
			}
			seen[p] = true
		}
	}

	if c.SourceBindings != nil {
		if len(c.SourceBindings) != len(c.SourceProjects) {
			return ErrCorpus
		}
		for _, p := range c.SourceProjects {
			if !workspace.ValidNotebookID(c.SourceBindings[p]) {
				return ErrCorpus
			}
		}
	}
	return nil
}
func (c Corpus) MarshalJSON() ([]byte, error) {
	if len(c.invalid) > 0 {
		return c.invalid, nil
	}
	return json.Marshal(corpusWire{NoteProjects: c.NoteProjects, Version: c.Version, Archives: c.Archives, Drafts: c.Drafts, History: c.History, SourceProjects: c.SourceProjects, SourceBindings: c.SourceBindings})
}
func (c *Corpus) UnmarshalJSON(raw []byte) error {
	*c = Corpus{}
	var wire corpusWire
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var fields map[string]json.RawMessage
	if d.Decode(&wire) != nil || d.Decode(new(any)) != io.EOF || json.Unmarshal(raw, &fields) != nil || string(fields["history"]) != "true" && string(fields["history"]) != "false" {
		c.invalid = append([]byte(nil), raw...)
		return nil
	}
	*c = Corpus{NoteProjects: wire.NoteProjects, SourceProjects: wire.SourceProjects, SourceBindings: wire.SourceBindings, Version: wire.Version, Archives: wire.Archives, Drafts: wire.Drafts, History: wire.History}
	if c.Validate() != nil {
		c.invalid = append([]byte(nil), raw...)
	}
	return nil
}
func (c *Corpus) ID() string {
	if c.Validate() != nil {
		return ""
	}
	raw, _ := json.Marshal(c)
	return Hash(raw)
}
