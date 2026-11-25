package web

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"testing"
)

func TestRetrievalSettingsHTTPDefaults(t *testing.T) {
	app, _ := fixture(t)
	response := call(app, "GET", "/api/knowledge/retrieval-settings", "", testToken)
	if response.Code != 200 {
		t.Fatalf("retrieval settings: %d %s", response.Code, response.Body)
	}
	var got struct {
		ID       string `json:"id"`
		Settings struct {
			Version    int `json:"version"`
			Search, AI struct {
				Version          int `json:"version"`
				Archives, Drafts string
			}
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.ID) != 64 || got.Settings.Version != 1 || got.Settings.Search.Archives != "exclude" || got.Settings.Search.Drafts != "include" || got.Settings.AI.Archives != "exclude" || got.Settings.AI.Drafts != "exclude" {
		t.Fatalf("wrong defaults: %+v", got)
	}
}

func TestContainerSearchHTTPFiltersBeforeLimits(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	group := workspace.EditGroup{ID: "archived-beacons", Summary: "Add archived beacons"}
	for i := 0; i < 105; i++ {
		group.Edits = append(group.Edits, workspace.NoteEdit{Path: fmt.Sprintf("a%03d.md", i), Content: "---\nfmn-container: archive\n---\n# Beacon\nArchived evidence\n"})
	}
	group.Edits = append(group.Edits, workspace.NoteEdit{Path: "z-ordinary.md", Content: "# Ordinary\nA weaker beacon match.\n"})
	// A group holds at most 50 edits.
	for edits := group.Edits; len(edits) > 0; edits = edits[min(50, len(edits)):] {
		group.ID, group.Edits = fmt.Sprintf("archived-beacons-%d", len(edits)), edits[:min(50, len(edits))]
		if _, err := store.ApplyGroup(ctx, group); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("literal exclusion before 100", func(t *testing.T) {
		response := call(app, "GET", "/api/notes?q=beacon&archives=exclude&drafts=exclude", "", testToken)
		var got []struct{ Path string }
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &got) != nil {
			t.Fatalf("literal: %d %s", response.Code, response.Body)
		}
		if len(got) != 1 || got[0].Path != "z-ordinary.md" {
			t.Fatalf("excluded notes consumed literal candidates: %+v", got)
		}
	})
	t.Run("default leaves Archives out", func(t *testing.T) {
		response := call(app, "POST", "/api/knowledge/hybrid-search", `{"text":"beacon","mode":"text","limit":5}`, testToken)
		var got struct{ Matches []struct{ Path string } }
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &got) != nil {
			t.Fatalf("hybrid: %d %s", response.Code, response.Body)
		}
		if len(got.Matches) != 1 || got.Matches[0].Path != "z-ordinary.md" {
			t.Fatalf("default search included Archives: %+v", got)
		}
	})
	t.Run("lower before branch and exact title", func(t *testing.T) {
		response := call(app, "POST", "/api/knowledge/hybrid-search", `{"text":"beacon","mode":"text","limit":5,"archives":"lower"}`, testToken)
		var got struct{ Matches []struct{ Path string } }
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &got) != nil {
			t.Fatalf("hybrid: %d %s", response.Code, response.Body)
		}
		if len(got.Matches) != 5 || got.Matches[0].Path != "z-ordinary.md" {
			t.Fatalf("lower-priority exact matches crowded out Notes: %+v", got)
		}
	})
}

func TestContainerHistoryHTTPUsesEachVersionsClassification(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	old, err := store.Save(ctx, "history.md", []byte("# Older ordinary\nolderneedle\n"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(ctx, old.Path, []byte("---\nfmn-container: archive\n---\n# Archived\narchivedneedle\n"), old.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	draft, err := store.Save(ctx, "promoted.md", []byte("---\nfmn-container: draft\n---\n# Former shared draft\ndraftneedle\n"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(ctx, draft.Path, []byte("# Promoted\nCurrent content\n"), draft.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		text, drafts string
		want         int
	}{{"olderneedle", "exclude", 1}, {"archivedneedle", "exclude", 0}, {"draftneedle", "exclude", 0}, {"draftneedle", "include", 1}} {
		t.Run(tc.text+tc.drafts, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"text": tc.text, "mode": "text", "scope": "history", "archives": "exclude", "drafts": tc.drafts})
			response := call(app, "POST", "/api/knowledge/hybrid-search", string(body), testToken)
			var got struct {
				Matches []struct{ Path, Revision string }
			}
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &got) != nil {
				t.Fatalf("history: %d %s", response.Code, response.Body)
			}
			if len(got.Matches) != tc.want {
				t.Fatalf("wrong version classification: %+v", got)
			}
			if tc.text == "olderneedle" && len(got.Matches) == 1 && got.Matches[0].Revision != old.Revision {
				t.Fatal("changed pinned citation")
			}
		})
	}
}

func TestContainerMCPPreferencesAndSettings(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	for _, note := range []struct{ path, content string }{{"archive.md", "---\nfmn-container: archive\n---\n# Beacon\n"}, {"draft.md", "---\nfmn-container: draft\n---\n# Beacon shared\n"}, {"ordinary.md", "# Beacon current\n"}} {
		if _, err := store.Save(ctx, note.path, []byte(note.content), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	client := actionClient(t, app, "@local-admin", "notes:read notes:write")
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "search_notes", Arguments: map[string]any{"project": "default", "query": "beacon", "archives": "exclude", "drafts": "exclude"}})
	if err != nil || result.IsError {
		t.Fatalf("scope arguments: %+v %v", result, err)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var entries []struct{ Path string }
	if json.Unmarshal(raw, &entries) != nil || len(entries) != 1 || entries[0].Path != "ordinary.md" {
		t.Fatalf("MCP did not enforce exclusions: %s", raw)
	}
	raw = actionCall(t, client, "get_retrieval_settings", map[string]any{"project": "default"}, false)
	var settings knowledge.RetrievalSettingsState
	if err = json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	settings.Settings.Search.Drafts = "exclude"
	actionCall(t, client, "set_retrieval_settings", map[string]any{"project": "default", "settings": settings.Settings, "expectedId": settings.ID}, false)
	actionCall(t, client, "set_retrieval_settings", map[string]any{"project": "default", "settings": settings.Settings, "expectedId": settings.ID}, true)
	for _, role := range []string{"read", "edit"} {
		if err = app.catalog.Member(catalog.Bootstrap, "default", role, role); err != nil {
			t.Fatal(err)
		}
		member := actionClient(t, app, role, "notes:read notes:write")
		actionCall(t, member, "get_retrieval_settings", map[string]any{"project": "default"}, false)
		actionCall(t, member, "set_retrieval_settings", map[string]any{"project": "default", "settings": settings.Settings, "expectedId": settings.ID}, true)
	}
}

func TestRetrievalSettingsHTTPAuthorityAndCAS(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"read", "edit", "owner"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", role, role); err != nil {
			t.Fatal(err)
		}
	}
	response := proxyCall(app, "read", "GET", "/p/default/api/knowledge/retrieval-settings", "")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	var original knowledge.RetrievalSettingsState
	if err := json.Unmarshal(response.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}
	settings := original.Settings
	settings.Search.Archives = "lower" // differs from the default, so the save changes the ID
	body, _ := json.Marshal(map[string]any{"settings": settings, "expectedId": original.ID})
	for _, role := range []string{"read", "edit"} {
		response = proxyCall(app, role, "POST", "/p/default/api/knowledge/retrieval-settings", string(body))
		if response.Code != 403 {
			t.Fatalf("%s changed defaults: %d", role, response.Code)
		}
	}
	response = proxyCall(app, "owner", "POST", "/p/default/api/knowledge/retrieval-settings", string(body))
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	response = proxyCall(app, "owner", "POST", "/p/default/api/knowledge/retrieval-settings", string(body))
	if response.Code != 409 {
		t.Fatal("stale defaults did not conflict", response.Code, response.Body)
	}
}
