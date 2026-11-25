package workspace

import (
	"slices"
	"testing"

	"forgetmenote/internal/attribution"
)

func TestTaskHistoryFindsEveryCommitNamingTheTask(t *testing.T) {
	s := newTestStore(t)
	ctx := attribution.With(t.Context(), attribution.Identity{ID: "alice", Name: "Alice", Email: "alice@example.test", Source: "mcp"})
	apply := func(summary string, changes map[string]string) {
		t.Helper()
		files := map[string][]byte{}
		for path, content := range changes {
			files[path] = []byte(content)
		}
		historyCommit(t, ctx, s, summary, nil, files)
	}
	apply("Task t-aaaa1111 added by p-alice", map[string]string{"people/p-bob.md": "# Tasks\n\n- One\n"})
	apply("Task t-aaaa11112 added by p-alice", map[string]string{"people/p-carol.md": "# Tasks\n\n- Lookalike\n"})
	apply("Task t-bbbb2222 added by p-alice", map[string]string{"people/p-bob.md": "# Tasks\n\n- One, edited\n"})
	// Reassigning moves the line between notes in one commit.
	apply("Task t-aaaa1111: reassigned to p-carol by p-bob", map[string]string{"people/p-bob.md": "# Tasks\n", "people/p-carol.md": "# Tasks\n\n- Lookalike\n- One, edited\n"})
	// A subject that only mentions the task elsewhere is not its history.
	apply("Note about Task t-aaaa1111", map[string]string{"people/p-carol.md": "# Tasks\n\n- Lookalike\n- One, edited\n\n"})

	history, err := s.TaskHistory(ctx, "t-aaaa1111")
	if err != nil {
		t.Fatal(err)
	}
	subjects := []string{}
	for _, c := range history {
		subjects = append(subjects, c.Message)
		if c.UserID != "alice" || c.AuthoredAt == "" || c.Revision == "" {
			t.Fatalf("history lost who and when: %+v", c)
		}
	}
	if want := []string{"Task t-aaaa1111: reassigned to p-carol by p-bob", "Task t-aaaa1111 added by p-alice"}; !slices.Equal(subjects, want) {
		t.Fatalf("history = %q, want %q", subjects, want)
	}
	if _, err = s.TaskHistory(ctx, "t-a; rm"); err == nil {
		t.Fatal("an invalid task name was searched")
	}
	if history, err = s.TaskHistory(ctx, "t-none0000"); err != nil || history == nil || len(history) != 0 {
		t.Fatalf("unknown task: %v %v", history, err)
	}
}
