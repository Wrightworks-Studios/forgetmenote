package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"forgetmenote/internal/workspace"
)

func (s *Server) importProject(w http.ResponseWriter, r *http.Request) {
	if !s.catalog.CanCreateProjects(subject(r.Context())) {
		fail(w, workspace.ErrForbidden)
		return
	}
	// One upload/restore at a time for the instance bounds temporary storage and
	// Git work without blocking ordinary notebook reads and saves.
	release, err := s.holdImport(r.Context())
	if errors.Is(err, errImportBusy) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Another notebook import is in progress. Try again shortly.", 429)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	defer release()
	// The server's read and write timeouts count from the start of every
	// request. The upload has the time uploadTime gives it instead; once it is
	// stored, the import bounds its own time in proportion to the bundle
	// (workspace.ImportBundleDB), so the write timeout must not cut its
	// answer short.
	controller := http.NewResponseController(w)
	controller.SetReadDeadline(time.Now().Add(uploadTime(r.ContentLength)))
	err = r.ParseMultipartForm(1 << 20)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			fail(w, workspace.ErrBundleTooLarge)
		} else {
			http.Error(w, "Choose a Git bundle to import.", 400)
		}
		return
	}
	form := r.MultipartForm
	if len(form.File) != 1 || len(form.File["bundle"]) != 1 || len(form.Value) != 2 || len(form.Value["id"]) != 1 || len(form.Value["name"]) != 1 {
		http.Error(w, "Provide a notebook name, ID, and one Git bundle.", 400)
		return
	}
	if form.File["bundle"][0].Size > workspace.MaxBundleBytes() {
		fail(w, workspace.ErrBundleTooLarge)
		return
	}
	file, err := form.File["bundle"][0].Open()
	if err != nil {
		fail(w, err)
		return
	}
	defer file.Close()
	controller.SetWriteDeadline(time.Time{})
	id := form.Value["id"][0]
	if err := s.catalog.Import(r.Context(), subject(r.Context()), id, form.Value["name"][0], file); err != nil {
		if errors.Is(err, workspace.ErrConflict) {
			http.Error(w, "A notebook with this ID already exists. Choose a different ID.", 409)
		} else {
			fail(w, err)
		}
		return
	}
	jsonResponse(w, map[string]string{"id": id})
}

// uploadTime is how long an import's upload of declared bytes may take to
// arrive: the minute the server's read timeout gives every request, and a
// second more for every 512 KiB, about the rate that timeout asks of a bundle
// at the default FMN_LIMIT_BUNDLE_BYTES. An unknown length counts as the
// largest upload the route admits.
func uploadTime(declared int64) time.Duration {
	largest := workspace.MaxBundleBytes() + 1<<20
	if declared < 0 || declared > largest {
		declared = largest
	}
	return time.Minute + time.Duration(declared>>19)*time.Second
}

var errImportBusy = errors.New("another notebook import is in progress; retry shortly")

// holdImport takes the instance's notebook import lock without waiting.
func (s *Server) holdImport(ctx context.Context) (func(), error) {
	release, ok, err := s.db.TryExclusive(ctx, "notebook-import")
	if err != nil {
		return nil, errors.New("notebook imports are unavailable; retry shortly")
	}
	if !ok {
		return nil, errImportBusy
	}
	return release, nil
}
