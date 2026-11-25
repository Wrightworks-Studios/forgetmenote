package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCommittedSnapshotDoesNotBlockSave(t *testing.T) {
	for _, scenario := range []struct{ scoped, handoff bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		scoped := scenario.scoped
		t.Run(fmt.Sprintf("scoped=%t/handoff=%t", scoped, scenario.handoff), func(t *testing.T) {
			store, held, reopen := newHeldTestStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			original := "Original idea\n\n" + strings.Repeat("Padding past the inline limit.\n", 400)
			initial, err := store.Save(ctx, "idea.md", []byte(original), "", "test")
			if err != nil {
				t.Fatal(err)
			}
			store = reopen()
			entered, release := held.arm()
			defer release()
			type result struct {
				snapshot Snapshot
				err      error
			}
			done := make(chan result, 1)
			go func() {
				var snapshot Snapshot
				var err error
				if scoped {
					snapshot, err = store.SnapshotScoped(ctx, "", ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}})
				} else {
					snapshot, err = store.Snapshot(ctx, "")
				}
				done <- result{snapshot, err}
			}()
			select {
			case <-entered:
			case r := <-done:
				t.Fatalf("snapshot missed barrier: %v", r.err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			saved := make(chan error, 1)
			go func() {
				if scenario.handoff {
					// The catalog fences a notebook it hands over by its generation.
					store.SetAuthorityGeneration(2)
					saved <- nil
				} else {
					_, err := store.Save(ctx, "idea.md", []byte("Updated idea\n"), initial.Revision, "test")
					saved <- err
				}
			}()
			select {
			case err := <-saved:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("mutation waited on immutable snapshot IO")
			}
			release()
			select {
			case r := <-done:
				if scenario.handoff {
					if !errors.Is(r.err, ErrConflict) || len(r.snapshot.Files) != 0 {
						t.Fatalf("stale authority snapshot disclosed: %+v %v", r.snapshot, r.err)
					}
					return
				}
				if r.err != nil || r.snapshot.Revision != initial.Revision || len(r.snapshot.Files) != 1 || r.snapshot.Files[0].Content != original {
					t.Fatalf("snapshot changed during save: %v %v", r.snapshot.Revision, r.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			current, err := store.Read(ctx, "idea.md", "")
			if err != nil || string(current.Content) != "Updated idea\n" {
				t.Fatalf("save lost: %v", err)
			}
		})
	}
}
