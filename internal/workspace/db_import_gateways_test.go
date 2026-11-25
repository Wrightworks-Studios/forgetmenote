package workspace

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/pg/pgtest"
)

// uploadThen runs beforeRead once, then reads from upload, or fails with err
// when upload is nil, as a client that disconnects does.
type uploadThen struct {
	beforeRead func()
	upload     io.Reader
	err        error
}

func (r *uploadThen) Read(p []byte) (int, error) {
	if r.beforeRead != nil {
		run := r.beforeRead
		r.beforeRead = nil
		run()
	}
	if r.upload == nil {
		return 0, r.err
	}
	return r.upload.Read(p)
}

// An import on one gateway that fails before it stores anything deletes
// nothing, though another gateway created the notebook while the import read
// its upload: only what an import stored is its to remove.
func TestFailedImportOnOneGatewayNeverDeletesANotebookAnotherCreated(t *testing.T) {
	for _, scenario := range []string{"damaged bundle", "disconnect"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			env := testEnv(t, s)
			a := s.noteStore
			gateway, _ := pgtest.Gateway(t, env.db)
			b := openTestNotes(t, &testNotebook{db: gateway, blobs: env.blobs})
			state := filepath.Join(t.TempDir(), "research")
			var created *Store
			upload := &uploadThen{err: io.ErrUnexpectedEOF, beforeRead: func() {
				var err error
				if created, err = CreateDB(ctx, a, "research", state, nil); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { created.Close() })
				if _, err = created.Save(ctx, "plan.md", []byte("# Plan\n\nOnly copy.\n"), "", "writer"); err != nil {
					t.Fatal(err)
				}
			}}
			if scenario == "damaged bundle" {
				upload.upload = bytes.NewReader([]byte("# not a Git bundle\n"))
			}
			if err := ImportBundleDB(ctx, b, "research", t.TempDir(), upload); err == nil {
				t.Fatal("an import of a failed upload succeeded")
			}
			fresh := openTestNotes(t, env)
			if raw, _, err := fresh.Notebook("research").Read(ctx, gitobj.ID{}, "plan.md"); err != nil || string(raw) != "# Plan\n\nOnly copy.\n" {
				t.Fatalf("the notebook gateway A created holds %q: %v", raw, err)
			}
			again, err := OpenDB(ctx, a, "research", state, nil)
			if err != nil {
				t.Fatal("reopening research on gateway A:", err)
			}
			again.Close()
		})
	}
}
