package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/workspace"
	"net/http"
	"strings"
	"testing"
)

func syntheticOrganizationAnswer(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Questions map[string]decision.Question `json:"questions"`
	}
	json.NewDecoder(r.Body).Decode(&request)
	answers := map[string]any{}
	for id, q := range request.Questions {
		if q.Type == "noul" {
			score := .02
			if id == "tag0" {
				score = .95
			}
			answers[id] = map[string]any{"type": "noul", "noul": score}
			continue
		}
		probabilities := map[string]float64{}
		for key := range q.Criteria.(map[string]any) {
			probabilities[key] = 0
		}
		probabilities["related"] = 1
		answers[id] = map[string]any{"type": "choice", "choice": "related", "confidence": 1, "probabilities": probabilities}
	}
	json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
}

func TestJevOrganizationTagsCreateGuardedProposalWithoutMutatingNotes(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	store, _ := app.catalog.Access("alice", "default", "edit")
	before, err := store.Read(context.Background(), "a.md", "")
	if err != nil {
		t.Fatal(err)
	}
	id := startReview(t, app, `{"kind":"organization","operationId":"organize","tags":["workshop"]}`)
	if job := waitReview(t, app, id); job["state"] != "completed" {
		t.Fatal(job)
	}
	after, err := store.Read(context.Background(), "a.md", "")
	if err != nil || workspace.ContentHash(before.Content) != workspace.ContentHash(after.Content) {
		t.Fatal("judgment changed a note without acceptance", err)
	}
	engine, releaseEngine, err := app.acquireEngine(t.Context(), store)
	if releaseEngine != nil {
		t.Cleanup(releaseEngine)
	}

	if err != nil {
		t.Fatal(err)
	}
	proposals, err := engine.Proposals(context.Background())
	if err != nil || len(proposals) == 0 {
		t.Fatalf("accepted tag has no guarded proposal: %+v %v", proposals, err)
	}
	var found bool
	for _, proposal := range proposals {
		for _, edit := range proposal.Group.Edits {
			meta, err := workspace.ParseMetadata([]byte(edit.Content))
			if err == nil && len(meta.Tags) == 1 && meta.Tags[0] == "workshop" && edit.Path == "a.md" {
				found = true
				if edit.Hash != workspace.ContentHash(before.Content) || edit.Revision == "" {
					t.Fatal("proposal lost current source guards")
				}
			}
		}
	}
	if !found {
		t.Fatal("tag proposal absent")
	}
}

func TestJevTagProposalPreservesMetadataBodyAndMultipleTags(t *testing.T) {
	for _, none := range []bool{false, true} {
		t.Run(map[bool]string{false: "multiple", true: "none"}[none], func(t *testing.T) {
			app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Questions map[string]decision.Question `json:"questions"`
				}
				json.NewDecoder(r.Body).Decode(&request)
				answers := map[string]any{}
				for id, q := range request.Questions {
					if q.Type == "noul" {
						score := .97
						if (id == "none") != none {
							score = .01
						}
						answers[id] = map[string]any{"type": "noul", "noul": score}
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
			})
			store, _ := app.catalog.Access("alice", "default", "edit")
			original := "---\r\n# retain this comment\r\ncustom: keep me\r\ntags: [legacy, Workshop]\r\n---\r\n# Isolated record\r\nUnchanged body with spacing.  \r\n"
			doc, err := store.Save(context.Background(), "isolated.md", []byte(original), "", "alice")
			if err != nil {
				t.Fatal(err)
			}
			id := startReview(t, app, `{"kind":"organization","operationId":"preserve","sources":[{"path":"isolated.md"}],"tags":["research","Research","workshop","fieldwork"]}`)
			job := waitReview(t, app, id)
			if job["state"] != "completed" {
				t.Fatal(job)
			}
			engine, releaseEngine, _ := app.acquireEngine(t.Context(), store)
			if releaseEngine != nil {
				t.Cleanup(releaseEngine)
			}

			proposals, err := engine.Proposals(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, proposal := range proposals {
				for _, edit := range proposal.Group.Edits {
					if edit.Path != "isolated.md" {
						t.Fatal("selected source proposal changed an unselected note")
					}
					found = true
					expected := strings.Replace(original, "tags: [legacy, Workshop]", "tags: [\"legacy\",\"Workshop\",\"fieldwork\",\"research\"]", 1)
					if edit.Content != expected || edit.Hash != workspace.ContentHash(doc.Content) {
						t.Fatalf("metadata/body/guards changed: %q", edit.Content)
					}
				}
			}
			if found == none {
				t.Fatalf("unexpected tag proposal presence: %v none=%v", found, none)
			}
			current, _ := store.Read(context.Background(), "isolated.md", "")
			if string(current.Content) != original {
				t.Fatal("proposal mutated source")
			}
		})
	}
}
