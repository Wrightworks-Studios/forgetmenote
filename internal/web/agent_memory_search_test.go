package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"forgetmenote/internal/catalog"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type searchedMemory struct {
	Core    recalledMemory  `json:"core"`
	Project *recalledMemory `json:"project"`
	Search  *struct {
		Mode  string `json:"mode"`
		Total int    `json:"total"`
	} `json:"search"`
}

// ranked lists the returned items' texts in rank order across core and project memory.
func (m searchedMemory) ranked(t *testing.T) []string {
	t.Helper()
	items := m.Core.Items
	if m.Project != nil {
		items = append(slices.Clone(items), m.Project.Items...)
	}
	texts := make([]string, len(items))
	for _, item := range items {
		if item.Rank < 1 || item.Rank > len(items) || texts[item.Rank-1] != "" {
			t.Fatalf("ranks are not 1..%d: %+v", len(items), m)
		}
		texts[item.Rank-1] = item.Text
	}
	return texts
}

func searchMemory(t *testing.T, session *mcp.ClientSession, args map[string]any) searchedMemory {
	t.Helper()
	var out searchedMemory
	if failure := callMemoryTool(t, session, "recall_memory", args, &out); failure != "" {
		t.Fatal("search:", failure)
	}
	if out.Search == nil {
		t.Fatalf("a recall with a query did not report its search: %+v", out)
	}
	return out
}

func rememberItems(t *testing.T, session *mcp.ClientSession, projectKey string, items ...map[string]any) rememberResult {
	t.Helper()
	args := map[string]any{"items": items}
	if projectKey != "" {
		args["projectKey"] = projectKey
	}
	var out rememberResult
	if failure := callMemoryTool(t, session, "remember", args, &out); failure != "" {
		t.Fatal("remember:", failure)
	}
	return out
}

// A query returns only the current items that match it, ranked across core and
// project memory, so an agent can look up what it needs without loading all of
// its memory. Retired items never come back.
func TestAgentMemorySearchReturnsOnlyMatchingItems(t *testing.T) {
	_, session, _ := agentMemoryFixture(t, true)
	const project = "github.com/example/dashboards"
	rememberItems(t, session, "",
		map[string]any{"kind": "fact", "text": "Weekly revenue questions use the finance data profile, grouped by market."},
		map[string]any{"kind": "lesson", "text": "The campaign export needs dates as YYYY-MM-DD."},
		map[string]any{"kind": "decision", "text": "Product photos are stored in the media notebook."},
		map[string]any{"kind": "preference", "text": "Shows revenue as a table, not a chart."},
	)
	retired := rememberItems(t, session, "", map[string]any{"kind": "fact", "text": "Revenue reports include refunds."})
	var confirmed confirmResult
	if failure := callMemoryTool(t, session, "confirm_memory", map[string]any{"id": retired.Items[0].ID, "result": "wrong", "reason": "finance excludes refunds"}, &confirmed); failure != "" || confirmed.Outcome != "retired" {
		t.Fatal("retire:", failure, confirmed)
	}
	rememberItems(t, session, project,
		map[string]any{"kind": "fact", "text": "Dashboard queries run against the reporting replica."},
		map[string]any{"kind": "decision", "text": "Revenue dashboards round to whole dollars."},
	)

	found := searchMemory(t, session, map[string]any{"query": "weekly revenue by market", "projectKey": project})
	got := found.ranked(t)
	if found.Search.Mode != "text" || found.Search.Total != 6 || len(got) != 3 || got[0] != "Weekly revenue questions use the finance data profile, grouped by market." || !slices.Contains(got, "Shows revenue as a table, not a chart.") || !slices.Contains(got, "Revenue dashboards round to whole dollars.") {
		t.Fatalf("search returned %v in mode %q of %d items", got, found.Search.Mode, found.Search.Total)
	}
	if found.Project == nil || found.Project.Notebook == "" || found.Core.Notebook == "" || len(found.Core.Changed) != 0 {
		t.Fatalf("search lost the files it came from or returned changes: %+v", found)
	}

	if none := searchMemory(t, session, map[string]any{"query": "quarterly forecast"}); len(none.ranked(t)) != 0 || none.Search.Total != 4 {
		t.Fatalf("an unrelated query returned items: %+v", none)
	}
	if limited := searchMemory(t, session, map[string]any{"query": "revenue", "projectKey": project, "limit": 1}).ranked(t); len(limited) != 1 {
		t.Fatalf("limit 1 returned %v", limited)
	}
	if everything := recallCore(t, session); len(everything.Items) != 4 {
		t.Fatalf("recall without a query no longer returns the whole memory: %+v", everything)
	}
	var out searchedMemory
	if failure := callMemoryTool(t, session, "recall_memory", map[string]any{"query": "revenue", "limit": 500}, &out); failure == "" {
		t.Fatal("an oversized limit was accepted")
	}
}

// Under the instance's adopted embedding policy, search also ranks by meaning:
// an item with no words in common with the query is found. Item vectors are
// reused, so a later search embeds only its query. Without the policy, or with the
// account's Memory setting off, memory text never reaches the embedding provider.
func TestAgentMemorySearchRanksByMeaningUnderInstancePolicy(t *testing.T) {
	app, id, session := agentMemoryApp(t)
	sent := memoryEmbeddingProvider(t, app)
	memoryInputs := func(all []string) (out []string) {
		for _, text := range all {
			if strings.Contains(text, "Sales figures") || strings.Contains(text, "Earnings calls") || strings.Contains(text, "Product photos") || strings.Contains(text, "ISO dates") {
				out = append(out, text)
			}
		}
		return out
	}
	rememberItems(t, session, "",
		map[string]any{"kind": "fact", "text": "Sales figures come from the finance profile."},
		map[string]any{"kind": "fact", "text": "Earnings calls happen each quarter."},
		map[string]any{"kind": "fact", "text": "Product photos live in the media notebook."},
		map[string]any{"kind": "lesson", "text": "The campaign export needs ISO dates."},
	)
	policy, err := app.catalog.SetAISettings(catalog.Bootstrap, catalog.AISettings{Embeddings: true, Model: "synthetic", Dimensions: 3})
	if err != nil {
		t.Fatal(err)
	}

	// Items far below the best match are left out, so a small memory is not
	// padded with unrelated items; one close to it stays.
	found := searchMemory(t, session, map[string]any{"query": "income"})
	if got := found.ranked(t); found.Search.Mode != "meaning" || !slices.Equal(got, []string{"Sales figures come from the finance profile.", "Earnings calls happen each quarter."}) {
		t.Fatalf("meaning search returned %v in mode %q", got, found.Search.Mode)
	}
	if embedded := sent(); !slices.Contains(embedded, "income") || len(memoryInputs(embedded)) != 4 {
		t.Fatalf("the first search did not embed its query and each item once: %q", embedded)
	}
	found = searchMemory(t, session, map[string]any{"query": "picture"})
	if got := found.ranked(t); !slices.Equal(got, []string{"Product photos live in the media notebook."}) {
		t.Fatalf("second meaning search returned %v", got)
	}
	if embedded := sent(); !slices.Contains(embedded, "picture") || len(memoryInputs(embedded)) != 0 {
		t.Fatalf("a later search embedded remembered items again: %q", embedded)
	}

	policy.Embeddings = false
	if policy, err = app.catalog.SetAISettings(catalog.Bootstrap, policy); err != nil {
		t.Fatal(err)
	}
	if off := searchMemory(t, session, map[string]any{"query": "income"}); off.Search.Mode != "text" || len(off.ranked(t)) != 0 {
		t.Fatalf("search without the embedding policy: %+v", off)
	}
	policy.Embeddings = true
	if _, err = app.catalog.SetAISettings(catalog.Bootstrap, policy); err != nil {
		t.Fatal(err)
	}
	settings, err := app.catalog.Assistant(id)
	if err != nil {
		t.Fatal(err)
	}
	settings.MaintainMemory = false
	if _, err = app.catalog.SaveAssistant(id, settings); err != nil {
		t.Fatal(err)
	}
	if off := searchMemory(t, session, map[string]any{"query": "photos"}); off.Search.Mode != "text" || len(off.ranked(t)) != 1 {
		t.Fatalf("search with Memory off: %+v", off)
	}
	if embedded := sent(); slices.Contains(embedded, "income") || slices.Contains(embedded, "photos") || len(memoryInputs(embedded)) != 0 {
		t.Fatalf("memory search reached the provider without the policy or the Memory setting: %q", embedded)
	}
}

// memoryEmbeddingProvider serves vectors by keyword under the instance's adopted
// embedding policy and returns every input it has received since the last call.
func memoryEmbeddingProvider(t *testing.T, app *Server) func() []string {
	t.Helper()
	var mu sync.Mutex
	var inputs []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model string
			Input []string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		inputs = append(inputs, in.Input...)
		mu.Unlock()
		vectors := make([][]float32, len(in.Input))
		for i, text := range in.Input {
			text = strings.ToLower(text)
			switch {
			case strings.Contains(text, "earnings"):
				vectors[i] = []float32{0.95, 0.312, 0}
			case strings.Contains(text, "sales") || strings.Contains(text, "income") || strings.Contains(text, "revenue"):
				vectors[i] = []float32{1, 0, 0}
			case strings.Contains(text, "photo") || strings.Contains(text, "picture"):
				vectors[i] = []float32{0, 1, 0}
			default:
				vectors[i] = []float32{0, 0, 1}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": in.Model, "embeddings": vectors})
	}))
	t.Cleanup(provider.Close)
	if err := app.ConfigureEmbeddings(provider.URL+"/api/embed", ""); err != nil {
		t.Fatal(err)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Clone(inputs)
		inputs = nil
		return out
	}
}

// A new item that nearly repeats a current one is still kept, since only the
// agent knows whether it updates or repeats it, and remember names the item it
// resembles so the agent can resolve the pair with confirm_memory.
func TestAgentMemoryRememberNamesTheItemANewOneRepeats(t *testing.T) {
	app, _, session := agentMemoryApp(t)
	memoryEmbeddingProvider(t, app)
	if _, err := app.catalog.SetAISettings(catalog.Bootstrap, catalog.AISettings{Embeddings: true, Model: "synthetic", Dimensions: 3}); err != nil {
		t.Fatal(err)
	}
	first := rememberItems(t, session, "", map[string]any{"kind": "fact", "text": "Revenue reports use the finance profile."})
	other := rememberItems(t, session, "", map[string]any{"kind": "fact", "text": "Product photos live in the media notebook."})
	if first.Items[0].Similar != "" || other.Items[0].Similar != "" {
		t.Fatalf("unrelated items were reported as similar: %+v %+v", first, other)
	}
	repeat := rememberItems(t, session, "", map[string]any{"kind": "fact", "text": "Revenue numbers come from the finance profile."})
	if len(repeat.Items) != 1 || repeat.Items[0].Duplicate || repeat.Items[0].Similar != first.Items[0].ID {
		t.Fatalf("a near repeat did not name the item it resembles: %+v", repeat)
	}
	if core := recallCore(t, session); len(core.Items) != 3 {
		t.Fatalf("a near repeat was not kept: %+v", core.Items)
	}
	var resolved confirmResult
	if failure := callMemoryTool(t, session, "confirm_memory", map[string]any{"id": first.Items[0].ID, "result": "changed", "replacement": "Revenue numbers come from the finance profile."}, &resolved); failure != "" || resolved.Outcome != "superseded" {
		t.Fatal("resolve:", failure, resolved)
	}
	core := recallCore(t, session)
	texts := []string{}
	for _, item := range core.Items {
		texts = append(texts, item.Text)
	}
	if !slices.Equal(texts, []string{"Product photos live in the media notebook.", "Revenue numbers come from the finance profile."}) || len(core.Changed) != 1 {
		t.Fatalf("resolving the pair left %v and changes %+v", texts, core.Changed)
	}
}

// With Memory on, an agent may keep a preference it inferred from the
// conversation. It is filed as the agent's, so it stays replaceable and is never
// mistaken for the user's own words.
func TestAgentMemoryKeepsInferredPreferencesAsTheAgents(t *testing.T) {
	_, session, store := agentMemoryFixture(t, true)
	saved := rememberItems(t, session, "", map[string]any{"kind": "preference", "text": "Wants revenue grouped by week."})
	core := recallCore(t, session)
	if len(core.Items) != 1 || core.Items[0].ID != saved.Items[0].ID || core.Items[0].Kind != "preference" || core.Items[0].From != "agent" || core.Items[0].Pinned {
		t.Fatalf("inferred preference: %+v", core.Items)
	}
	doc, err := store.Read(t.Context(), "memory.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc.Content), "## Preferences\n- Wants revenue grouped by week.\n  `"+saved.Items[0].ID+" · preference · from: agent, ") {
		t.Fatalf("committed memory:\n%s", doc.Content)
	}
	var changed confirmResult
	if failure := callMemoryTool(t, session, "confirm_memory", map[string]any{"id": saved.Items[0].ID, "result": "changed", "replacement": "Wants revenue grouped by month."}, &changed); failure != "" || changed.Outcome != "superseded" {
		t.Fatal("change:", failure, changed)
	}
	if core = recallCore(t, session); len(core.Items) != 1 || core.Items[0].Kind != "preference" || core.Items[0].From != "agent" {
		t.Fatalf("a changed inferred preference lost its kind: %+v", core.Items)
	}
}
