package quicknotes

import (
	"context"
	"errors"
	"os"
	"path"

	"forgetmenote/internal/workspace"
)

func parentFolder(note string) string {
	folder := path.Dir(note)
	if folder == "." {
		return ""
	}
	return folder
}

func (s *Service) File(ctx context.Context, subject, id string, destination Destination, version string) (Capture, error) {
	return s.file(ctx, subject, id, destination, version, false, 0)
}
func (s *Service) file(ctx context.Context, subject, id string, destination Destination, version string, automatic bool, generation uint64) (Capture, error) {
	if !validID(id) {
		return Capture{}, workspace.ErrForbidden
	}
	ctx, account, err := s.actor(ctx, subject)
	if err != nil {
		return Capture{}, err
	}
	lock := s.objectLock(account.ID + "/" + id)
	lock.Lock()
	defer lock.Unlock()
	r, err := s.readRecord(account.ID, id)
	if err != nil {
		return Capture{}, err
	}
	if r.Pending != nil {
		return s.resume(ctx, subject, &r)
	}
	if r.Status == "filed" && r.Project == destination.Project && parentFolder(r.Path) == destination.Folder {
		return s.view(ctx, subject, r)
	}
	if r.Status != "inbox" && r.Status != "suggested" {
		return Capture{}, workspace.ErrConflict
	}
	if err = s.verifyInbox(subject, r); err != nil {
		return Capture{}, err
	}
	destination, err = s.destination(ctx, subject, destination)
	if err != nil {
		return Capture{}, err
	}
	source, err := s.catalog.Access(subject, r.Project, "owner")
	if err != nil {
		return Capture{}, err
	}
	doc, err := source.Read(ctx, r.Path, "")
	if err != nil {
		return Capture{}, err
	}
	if doc.Version != version || automatic && r.RoutingHoldHash == workspace.ContentHash(doc.Content) {
		return Capture{}, workspace.ErrConflict
	}
	op, err := newOperation()
	if err != nil {
		return Capture{}, err
	}
	in := workspace.CaptureMove{ID: op, From: r.Path, Version: doc.Version, Hash: workspace.ContentHash(doc.Content), To: path.Join(destination.Folder, path.Base(r.InboxPath)), SourceProject: r.Project, TargetProject: destination.Project}
	if in.SourceProject == in.TargetProject && in.From == in.To {
		return s.view(ctx, subject, r)
	}
	r.clearRetry()
	r.RoutingHoldHash = ""
	r.Pending = &transfer{Intent: in, Automatic: automatic, SettingsGeneration: generation}
	r.Holder = s.db.Holder
	r.Status = "filing"
	r.Suggestion = nil
	r.Error = ""
	if err = s.writeRecord(&r); err != nil {
		return Capture{}, err
	}
	return s.resume(ctx, subject, &r)
}
func (s *Service) Undo(ctx context.Context, subject, id string) (Capture, error) {
	if !validID(id) {
		return Capture{}, workspace.ErrForbidden
	}
	ctx, account, err := s.actor(ctx, subject)
	if err != nil {
		return Capture{}, err
	}
	lock := s.objectLock(account.ID + "/" + id)
	lock.Lock()
	defer lock.Unlock()
	r, err := s.readRecord(account.ID, id)
	if err != nil {
		return Capture{}, err
	}
	if r.Pending != nil {
		return s.resume(ctx, subject, &r)
	}
	if r.Status != "filed" {
		return Capture{}, workspace.ErrConflict
	}
	if err = s.verifyInbox(subject, r); err != nil {
		return Capture{}, err
	}
	source, err := s.catalog.Access(subject, r.Project, "owner")
	if err != nil {
		return Capture{}, err
	}
	doc, err := source.Read(ctx, r.Path, "")
	if err != nil {
		return Capture{}, err
	}
	if doc.Version != r.FiledVersion {
		return Capture{}, workspace.ErrConflict
	}
	if _, err = s.catalog.Access(subject, r.InboxProject, "owner"); err != nil {
		return Capture{}, err
	}
	op, err := newOperation()
	if err != nil {
		return Capture{}, err
	}
	r.clearRetry()
	r.Holder = s.db.Holder
	r.Pending = &transfer{Undo: true, Intent: workspace.CaptureMove{ID: op, From: r.Path, Version: doc.Version, Hash: workspace.ContentHash(doc.Content), To: r.InboxPath, SourceProject: r.Project, TargetProject: r.InboxProject}}
	r.Status = "undoing"
	r.Error = ""
	if err = s.writeRecord(&r); err != nil {
		return Capture{}, err
	}
	return s.resume(ctx, subject, &r)
}
func (s *Service) Recover(ctx context.Context, subject, id string) (Capture, error) {
	if !validID(id) {
		return Capture{}, workspace.ErrForbidden
	}
	ctx, account, err := s.actor(ctx, subject)
	if err != nil {
		return Capture{}, err
	}
	lock := s.objectLock(account.ID + "/" + id)
	lock.Lock()
	defer lock.Unlock()
	r, err := s.readRecord(account.ID, id)
	if err != nil {
		return Capture{}, err
	}
	if err = s.verifyInbox(subject, r); err != nil {
		return Capture{}, err
	}
	if r.Status == "saving" {
		store, e := s.catalog.Access(subject, r.InboxProject, "owner")
		if e != nil {
			return Capture{}, e
		}
		doc, e := store.CaptureSaved(ctx, r.ID, r.InboxPath, r.SaveHash)
		if e != nil {
			if errors.Is(e, os.ErrNotExist) && !r.AwaitingClient {
				// The gateway holds only an idempotence record, never the draft.
				// Keep it so the original client can retry the identical payload.
				r.AwaitingClient = true
				if writeErr := s.writeRecord(&r); writeErr != nil {
					return Capture{}, writeErr
				}
			}
			return Capture{}, e
		}
		r.Version = doc.Version
		r.Status = "inbox"
		r.Error = ""
		r.clearRetry()
		if e = s.writeRecord(&r); e != nil {
			return Capture{}, e
		}
	}
	if r.Pending != nil {
		return s.resume(ctx, subject, &r)
	}
	return s.view(ctx, subject, r)
}
func (s *Service) transferAccess(subject string, r record) (*workspace.Store, *workspace.Store, error) {
	if err := s.verifyInbox(subject, r); err != nil {
		return nil, nil, err
	}
	source, err := s.catalog.Access(subject, r.Pending.Intent.SourceProject, "owner")
	if err != nil {
		return nil, nil, err
	}
	target, err := s.catalog.Access(subject, r.Pending.Intent.TargetProject, "owner")
	return source, target, err
}
func (s *Service) resume(ctx context.Context, subject string, r *record) (Capture, error) {
	source, target, err := s.transferAccess(subject, *r)
	if err != nil {
		return Capture{}, err
	}
	in := r.Pending.Intent
	preflight, err := target.CapturePreflight(ctx, in)
	if err != nil {
		return s.abort(ctx, subject, r, source, target, err)
	}
	if preflight.State == "received" {
		filed, e := target.Read(ctx, in.To, preflight.Revision)
		if e != nil {
			return Capture{}, e
		}
		return s.complete(ctx, subject, r, filed)
	}
	if r.Pending.Automatic {
		if err = s.automaticAllowed(ctx, subject, *r); err != nil {
			return s.abort(ctx, subject, r, source, target, err)
		}
	}
	doc, err := source.CapturePrepare(ctx, in)
	if err != nil {
		return s.abort(ctx, subject, r, source, target, err)
	}
	// Recheck catalog authority after remote preparation and immediately before the
	// publishing write. Source-owner reservation protects its exact content meanwhile.
	source, target, err = s.transferAccess(subject, *r)
	if err != nil {
		return Capture{}, err
	}
	if r.Pending.Automatic {
		if err = s.automaticAllowed(ctx, subject, *r); err != nil {
			return s.abort(ctx, subject, r, source, target, err)
		}
	}
	filed, err := target.CaptureReceive(ctx, in, doc.Content)
	if err != nil {
		outcome, e := target.CaptureAbortReceive(ctx, in)
		if e != nil {
			return Capture{}, e
		}
		if outcome.State != "received" {
			return s.abort(ctx, subject, r, source, target, err)
		}
		filed, e = target.Read(ctx, in.To, outcome.Revision)
		if e != nil {
			return Capture{}, e
		}
	}
	return s.complete(ctx, subject, r, filed)
}
func (s *Service) complete(ctx context.Context, subject string, r *record, filed workspace.Document) (Capture, error) {
	in := r.Pending.Intent
	if filed.Path != in.To || workspace.ContentHash(filed.Content) != in.Hash {
		return Capture{}, workspace.ErrConflict
	}
	source, _, err := s.transferAccess(subject, *r)
	if err != nil {
		return Capture{}, err
	}
	if _, err = source.CaptureFinish(ctx, in); err != nil {
		return Capture{}, err
	}
	r.Project = in.TargetProject
	r.Path = in.To
	r.Version = filed.Version
	r.Hash = workspace.ContentHash(filed.Content)
	r.Error = ""
	r.Suggestion = nil
	if r.Pending.Undo {
		// Persist the user's reversal with its restored bytes in the same
		// checkpoint as completion, including recovery after a lost response.
		r.RoutingHoldHash = r.Hash
		r.Status = "inbox"
		r.FiledVersion = ""
	} else {
		r.Status = "filed"
		r.FiledVersion = filed.Version
	}
	r.Pending = nil
	r.clearRetry()
	if err = s.writeRecord(r); err != nil {
		return Capture{}, err
	}
	return s.view(ctx, subject, *r)
}
func (s *Service) abort(ctx context.Context, subject string, r *record, source, target *workspace.Store, cause error) (Capture, error) {
	// A successful abort on the destination fences requests still in flight. Never
	// release a source reservation merely because a transport response was lost.
	outcome, err := target.CaptureAbortReceive(ctx, r.Pending.Intent)
	if err != nil {
		return Capture{}, err
	}
	if outcome.State == "received" {
		filed, e := target.Read(ctx, r.Pending.Intent.To, outcome.Revision)
		if e != nil {
			return Capture{}, e
		}
		return s.complete(ctx, subject, r, filed)
	}
	if _, err = source.CaptureCancel(ctx, r.Pending.Intent); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Capture{}, err
	}
	if r.Pending.Undo {
		r.Status = "filed"
	} else {
		r.Status = "inbox"
	}
	r.Pending = nil
	r.clearRetry()
	r.Error = "Filing was not completed; the original note is unchanged."
	if err = s.writeRecord(r); err != nil {
		return Capture{}, err
	}
	return Capture{}, cause
}
