package web

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSourceRetrievalRequiresExplicitNotebookScope(t *testing.T) {
	app, _ := fixture(t)
	r := httptest.NewRequest("POST", "http://localhost/api/retrieval", strings.NewReader(`{"query":"startup","corpora":["repository"]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing notebook scope: got %d %s; want 400", w.Code, w.Body.String())
	}
}

func TestSourceRetrievalRejectsUnknownCorpus(t *testing.T) {
	app, _ := fixture(t)
	r := httptest.NewRequest("POST", "http://localhost/api/retrieval", strings.NewReader(`{"projects":["default"],"query":"startup","corpora":["unknown"]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid corpus: got %d %s; want 400", w.Code, w.Body.String())
	}
}

func TestSourceSearchChatScopeCannotExpand(t *testing.T) {
	app, _ := fixture(t)
	scope := &chat.RunScope{Project: "default", Run: chat.Run{Options: chat.Options{Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", SourceProjects: []string{"allowed"}}}}}
	ctx := chat.WithScope(context.Background(), scope)
	if err := app.checkSourceChatScope(ctx, "default"); !errors.Is(err, chat.ErrOutsideCorpus) {
		t.Fatalf("current notebook was not selected for source: %v", err)
	}
	raw := json.RawMessage(`{"projects":["outside"],"query":"password"}`)
	if _, err := scopeChatActionArguments(ctx, "search_repository_sources", raw); !errors.Is(err, chat.ErrOutsideCorpus) {
		t.Fatalf("model widened run source scope: %v", err)
	}
}

func TestChatRepositoryActionsBindOnlySelectedNotebooks(t *testing.T) {
	app, _ := fixture(t)
	corpus := &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", SourceProjects: []string{"allowed"}}
	ctx := chat.WithScope(t.Context(), &chat.RunScope{Project: "default", Run: chat.Run{Options: chat.Options{Corpus: corpus}}})
	for _, name := range []string{"repository_source_status", "repository_source_tree", "repository_source_map", "repository_source_file", "search_repository_sources"} {
		t.Run(name, func(t *testing.T) {
			args := map[string]any{"project": "allowed"}
			if name == "repository_source_file" {
				args["path"], args["revision"] = "health.go", strings.Repeat("a", 40)
			}
			if name == "search_repository_sources" {
				args = map[string]any{"projects": []string{"allowed"}, "query": "health"}
			}
			raw, _ := json.Marshal(args)
			_, bound, err := app.actionArguments("default", name, raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = scopeChatActionArguments(ctx, name, bound); err != nil {
				t.Fatal(err)
			}
			if name == "search_repository_sources" {
				args["projects"] = []string{"outside"}
			} else {
				args["project"] = "outside"
			}
			raw, _ = json.Marshal(args)
			if _, err = scopeChatActionArguments(ctx, name, raw); !errors.Is(err, chat.ErrOutsideCorpus) {
				t.Fatalf("model widened source scope: %v", err)
			}
		})
	}
	if _, _, err := app.actionArguments("default", "read_note", json.RawMessage(`{"project":"allowed","path":"note.md"}`)); err == nil {
		t.Fatal("source selection granted cross-notebook note access")
	}
}

func TestSourceRetrievalRejectsUnreadableNotebookWithoutMetadata(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "reader", "read"); err != nil {
		t.Fatal(err)
	}
	response := proxyCall(app, "reader", "POST", "/api/retrieval", `{"projects":["default","hidden"],"corpora":["repository"],"query":"health"}`)
	if response.Code != 403 && response.Code != 404 {
		t.Fatal(response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "downloaded") {
		t.Fatal("denied source metadata disclosed")
	}
}
