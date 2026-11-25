package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"

	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/notestore"
)

// ErrBundleTimeout is an import that ran out of time, which does not show
// that its bundle is damaged: the same bundle may import when the server is
// less busy.
var ErrBundleTimeout = gitexport.ErrBundleTimeout

// A bundle import, its check and the storing of its history together, may
// take importTimeout and one second more for every importRate bytes of the
// bundle, counted once its whole upload is read: the time an upload takes to
// arrive is the caller's. The floor is the 90 s every import had before; per
// byte the import has twice the time gitexport gives the bundle's Git check
// (a second per 2 MiB), so storing a large checked history has as long as
// its check had. F2's 952 MB bundle has about 17 minutes, its check 9.
var (
	importTimeout       = 90 * time.Second
	importRate    int64 = 1 << 20
	// importAfter runs f once an import's time limit has passed; a test ends
	// that time when the import has reached the step it checks.
	importAfter = time.AfterFunc
)

// importLimit is how long the import of a bundle of size bytes may take once
// its upload is read.
func importLimit(size int64) time.Duration {
	return importTimeout + time.Duration(float64(size)/float64(importRate)*float64(time.Second))
}

// importClock reads an import's upload and starts the import's time limit
// once it has read all of it, when the limit cancels the import with a
// timeout.
type importClock struct {
	upload io.Reader
	size   int64
	cancel context.CancelCauseFunc
	timer  *time.Timer
}

func (c *importClock) Read(p []byte) (int, error) {
	n, err := c.upload.Read(p)
	c.size += int64(n)
	if err == io.EOF && c.timer == nil {
		limit := importLimit(c.size)
		c.timer = importAfter(limit, func() {
			c.cancel(&refusal{fmt.Sprintf("the notebook import ran out of time after %v, which does not show that the bundle is damaged; import it again when the server is less busy", limit.Round(time.Second)), ErrBundleTimeout})
		})
	}
	return n, err
}

// stop ends the clock of an import that has returned.
func (c *importClock) stop() {
	if c.timer != nil {
		c.timer.Stop()
	}
}

// importFailed is the error of an import that failed with err. Once ctx is
// done the import may have failed for that alone: then the import's own time
// limit, or the caller's deadline, is a timeout, as gitexport reports one
// during the bundle's check, and not the error of whatever step it stopped.
func importFailed(ctx context.Context, err error) error {
	if ctx.Err() == nil || errors.Is(err, ErrBundleTimeout) {
		return err
	}
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, ErrBundleTimeout):
		return cause
	case errors.Is(cause, context.DeadlineExceeded):
		return &refusal{"the notebook import ran out of the caller's time, which does not show that the bundle is damaged; import it again when the server is less busy", ErrBundleTimeout}
	}
	return err
}

// imports holds the notebooks this process is importing, so a second import
// of one is a conflict before it reads its upload.
var imports struct {
	sync.Mutex
	running map[importKey]bool
}

type importKey struct {
	notes    *notestore.Store
	notebook string
}

// ImportBundleDB restores notebook, which notes must not hold yet, from the
// complete Git bundle input; the catalog then opens it with OpenDB. It
// imports the history of the bundle's HEAD, every commit keeping its ID, and
// refuses with ErrBundle or ErrBundleTooLarge, before storing anything, an
// upload over FMN_LIMIT_BUNDLE_BYTES, an incomplete or damaged bundle,
// history over the import limits, or a HEAD with a file that is not a
// notebook file, exceeds its budget or repeats an fmn-id. A notebook that
// notes holds, or another import of it in this process, is an ErrConflict
// error. staging is a private directory the caller supplies for
// the upload, never a user-selected path.
//
// Once its whole upload is read, the import may take 90 seconds and one more
// for every MiB of the bundle (importLimit); running out of that time, or of
// ctx's deadline, is an ErrBundleTimeout error, never a refusal of the bundle.
//
// An import that fails after storing part of the history, or leaves a
// notebook OpenDB refuses, such as one with an invalid Trash index or
// archive, deletes the notebook its first commit created and, through
// RemoveExport, the Git export the exporter may already have made of it, so
// importing again succeeds and the new notebook backs up. It deletes only
// that incarnation of the notebook (notestore.Store.DeleteIncarnation): an
// import that failed before storing anything, such as a damaged upload, one
// whose client disconnected, or one that ran out of time during its check,
// deletes nothing, so a notebook another gateway created under the ID
// meanwhile stays. The catalog also reserves the ID on every gateway while
// it imports (Catalog.Import). Only HEAD's history is kept: the bundle's
// other branches and tags are checked and left out.
func ImportBundleDB(ctx context.Context, notes *notestore.Store, notebook, staging string, input io.Reader) error {
	if !ValidNotebookID(notebook) {
		return errors.New("invalid notebook ID")
	}
	key := importKey{notes, notebook}
	imports.Lock()
	if imports.running[key] {
		imports.Unlock()
		return fmt.Errorf("%w: notebook %s is being imported", ErrConflict, notebook)
	}
	if imports.running == nil {
		imports.running = map[importKey]bool{}
	}
	imports.running[key] = true
	imports.Unlock()
	defer func() {
		imports.Lock()
		delete(imports.running, key)
		imports.Unlock()
	}()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	clock := &importClock{upload: input, cancel: cancel}
	defer clock.stop()
	nb := notes.Notebook(notebook)
	if _, err := nb.Head(ctx); err == nil {
		return importConflict(notebook)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var created int64 // the incarnation this import's first commit created
	_, err := gitexport.ImportBundle(ctx, nb, clock, gitexport.Limits{
		FileBytes: func(path string) int64 { return int64(MaxReadBytes(path)) },
		Allowed:   notebookFile,
		NoteID:    importNoteID,
		Staging:   staging,
		Created:   func(incarnation int64) { created = incarnation },
	})
	if err == nil {
		err = checkImportOpens(ctx, notes, notebook, staging)
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, gitexport.ErrNotEmpty) || errors.Is(err, fs.ErrExist) {
		return importConflict(notebook) // created meanwhile, by another
	}
	err = importFailed(ctx, err)
	if created == 0 {
		return err // it stored nothing
	}
	// A running exporter may have exported what this import stored already;
	// that export would diverge from the next notebook of this ID.
	removeCtx, done := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer done()
	if removed := notes.DeleteIncarnation(removeCtx, notebook, created); errors.Is(removed, fs.ErrNotExist) {
		return err // deleted, or someone else's since
	} else if removed != nil {
		return errors.Join(err, fmt.Errorf("remove the failed import of notebook %s: %w", notebook, removed))
	}
	if removed := RemoveExport(removeCtx, notes, notebook); removed != nil {
		return errors.Join(err, fmt.Errorf("remove the export of the failed import of notebook %s: %w", notebook, removed))
	}
	return err
}

func importConflict(notebook string) error {
	return fmt.Errorf("%w: notebook %s already exists", ErrConflict, notebook)
}

// importNoteID applies a bundle's identity rule: a malformed fmn-id field
// refuses the bundle, while legacy front matter that only mentions fmn-id is
// a note without one.
func importNoteID(content []byte) (string, error) {
	id, err := NoteID(content)
	if err != nil && !hasFrontMatterField(content, "fmn-id") {
		return "", nil
	}
	return id, err
}

// checkImportOpens opens the imported notebook once, as the catalog does
// before publishing it, so an import never succeeds with a notebook the
// catalog could not serve.
func checkImportOpens(ctx context.Context, notes *notestore.Store, notebook, staging string) error {
	state, err := os.MkdirTemp(staging, "fmn-open-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(state)
	s, err := OpenDB(ctx, notes, notebook, state, nil)
	var damage *damagedTrash
	if errors.As(err, &damage) {
		// The bundle brought the damage: it is the caller's input.
		return invalidInput(fmt.Sprintf("the bundle's Trash is invalid (%s: %v)", damage.what, damage.reason))
	}
	if err != nil {
		return err
	}
	return s.Close()
}
