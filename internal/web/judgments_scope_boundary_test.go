package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/workspace"
)

func TestJudgmentReviewHTTPExcludesContainersAndUncommittedLiveText(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var leaked atomic.Bool
	configureSyntheticJudgments(t, app, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if bytes.Contains(raw, []byte("EXCLUDED_")) {
			leaked.Store(true)
		}
		var request struct {
			Questions map[string]decision.Question `json:"questions"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		answers := map[string]any{}
		for id, question := range request.Questions {
			probabilities := map[string]float64{}
			for key := range question.Criteria.(map[string]any) {
				probabilities[key] = 0
			}
			probabilities["compatible"] = 1
			answers[id] = map[string]any{"type": "choice", "choice": "compatible", "confidence": 1, "probabilities": probabilities}
		}
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
	})
	ctx := context.Background()
	store, err := app.catalog.Access("alice", "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	committed := "# Workshop\nThe workshop opens at 09:00 on weekdays.\n"
	for _, note := range []struct{ path, content string }{
		{"notes/a.md", committed},
		{"notes/b.md", "# Workshop\nThe workshop closes at 17:00 on weekdays.\n"},
		{"archive.md", "---\nfmn-container: archive\n---\n# Workshop\nEXCLUDED_ARCHIVE opens at noon.\n"},
		{"draft.md", "---\nfmn-container: draft\n---\n# Workshop\nEXCLUDED_DRAFT opens at noon.\n"},
		{"discarded.md", "# Workshop\nEXCLUDED_TRASH opens at noon.\n"},
	} {
		if _, err := store.Save(ctx, note.path, []byte(note.content), "", "alice"); err != nil {
			t.Fatal(err)
		}
	}
	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	trashed, err := store.TrashNotes(ctx, []string{"discarded.md"}, status.Revision, "alice")
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveRead(ctx, "notes/a.md")
	if err != nil {
		t.Fatal(err)
	}
	private := "# Workshop\nEXCLUDED_LIVE changes the hours but is not committed.\n"
	if _, err := store.LivePatch(ctx, workspace.TargetedPatch{Path: "notes/a.md", Base: live.LiveRevision, OperationID: "scope-uncommitted", Edits: []workspace.TextEdit{{Start: 0, End: len(committed), Old: committed, Text: private}}}); err != nil {
		t.Fatal(err)
	}
	if w := proxyCall(app, "alice", "POST", "/api/judgments/settings", `{"consent":true,"rerank":true,"citations":true}`); w.Code != 200 {
		t.Fatalf("consent: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"archive.md", "draft.md", trashed.Entries[0].Path} {
		body, _ := json.Marshal(map[string]any{"kind": "consistency", "operationId": "excluded-" + path, "sources": []map[string]string{{"path": path}}})
		w := proxyCall(app, "alice", "POST", "/p/default/api/judgments/jobs", string(body))
		if w.Code != http.StatusForbidden {
			t.Fatalf("explicit excluded source: %s status=%d", path, w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("disallowed source requests performed paid work")
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/judgments/jobs", `{"kind":"consistency","operationId":"active-scope"}`)
	if w.Code != 200 {
		t.Fatalf("review create: %d %s", w.Code, w.Body.String())
	}
	var job struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w = proxyCall(app, "alice", "GET", "/p/default/api/judgments/jobs/"+job.ID, "")
		if w.Code != 200 {
			t.Fatalf("review read: %d %s", w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if job.State == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.State != "completed" || calls.Load() != 3 || leaked.Load() {
		t.Fatalf("review scope: state=%s calls=%d excludedContent=%v", job.State, calls.Load(), leaked.Load())
	}
	doc, err := store.KnowledgeRead(ctx, "notes/a.md", "")
	if err != nil || string(doc.Content) != committed {
		t.Fatalf("review changed committed note: %v", err)
	}
	live, err = store.LiveRead(ctx, "notes/a.md")
	if err != nil || !strings.Contains(live.Content, "EXCLUDED_LIVE") {
		t.Fatalf("review discarded pending live work: %v", err)
	}
}
