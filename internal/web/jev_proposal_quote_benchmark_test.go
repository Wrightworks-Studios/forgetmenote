package web

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"strings"
	"testing"
)

func BenchmarkRetainedProposalQuotes(b *testing.B) {
	group := workspace.EditGroup{ID: "bounded-proposal", Edits: []workspace.NoteEdit{{Path: "notes/source.md", Revision: "revision", Hash: "hash", Content: strings.Repeat("proposed text ", 80000)}}}
	raw, _ := json.Marshal(group)
	proposal := knowledge.Proposal{ID: "proposal", Status: "proposed", RequestHash: workspace.ContentHash(raw), Group: group}
	result, _ := json.Marshal(proposal)
	// Retained preimages are not needed to match proposed quotations.
	var fields map[string]json.RawMessage
	json.Unmarshal(result, &fields)
	fields["before"], _ = json.Marshal(map[string]string{"notes/source.md": strings.Repeat("original text ", 80000)})
	result, _ = json.Marshal(fields)
	arguments, _ := json.Marshal(map[string]any{"project": "default", "mode": "proposal", "group": group})
	run := chat.Run{Operations: []chat.Operation{{Name: "submit_change_group", State: "completed", Result: result, Arguments: arguments}}}
	evidence := knowledge.JudgmentEvidence{Path: "notes/source.md", Revision: "revision", Hash: "hash"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		match := retainedProposalQuoteMatcher(b.Context(), run, "default")
		for j := 0; j < 32; j++ {
			if found, err := match(evidence, "proposed text"); err != nil || !found {
				b.Fatal("missing proposed quotation")
			}
		}
	}
}

func TestRetainedProposalQuoteCancellation(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(map[bool]string{false: "before preparation", true: "after preparation"}[prepared], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			match := retainedProposalQuoteMatcher(ctx, chat.Run{}, "default")
			evidence := knowledge.JudgmentEvidence{Path: "notes/source.md"}
			if prepared {
				if found, err := match(evidence, "missing"); err != nil || found {
					t.Fatalf("found=%v err=%v", found, err)
				}
			}
			cancel()
			if found, err := match(evidence, "missing"); found || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled match: found=%v err=%v", found, err)
			}
		})
	}
}
