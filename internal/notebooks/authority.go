package notebooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/backups"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/workspace"
	"log"
	"strings"
)

// Authority owns the serialized, recoverable transaction across the catalog,
// notebook store, verified source, and external backup writer. Each notebook's
// handoff runs under its HandoffLock, so one gateway at a time applies,
// cancels or recovers it.
type Authority struct {
	db      *pg.DB
	catalog *catalog.Catalog
	access  func(context.Context, string, string) (*workspace.Store, error)
	check   func(context.Context) error
	guard   func(context.Context, string, bool) error
}
type Managers struct {
	Repositories *reposource.Manager
	Backups      *backups.Manager
}

func NewAuthority(db *pg.DB, c *catalog.Catalog, access func(context.Context, string, string) (*workspace.Store, error), check func(context.Context) error, guard func(context.Context, string, bool) error) *Authority {
	return &Authority{db: db, catalog: c, access: access, check: check, guard: guard}
}

// HandoffLock names the lock a notebook's authority handoff runs under.
func HandoffLock(project string) string { return "authority-handoff " + project }

// hold takes project's handoff lock, refusing while another call holds it.
func (s *Authority) hold(ctx context.Context, project string) (func(), error) {
	release, ok, err := s.db.TryExclusive(ctx, HandoffLock(project))
	if err != nil {
		return nil, errors.Join(errors.New("authority handoffs are unavailable; retry shortly"), err)
	}
	if !ok {
		return nil, workspace.ErrConflict
	}
	return release, nil
}

type PreviewInput struct {
	Project                string `json:"project"`
	To                     string `json:"to"`
	Repository             string `json:"repository,omitempty"`
	OperatorWritersStopped bool   `json:"operatorWritersStopped,omitempty"`
}
type ApplyInput struct {
	Project string `json:"project"`
	Preview string `json:"preview"`
}

func snapshotHash(snapshot workspace.NotebookSnapshot) string {
	raw, _ := json.Marshal(snapshot.Files)
	return workspace.ContentHash(raw)
}
func (s *Authority) Preview(ctx context.Context, in PreviewInput, managers Managers) (catalog.AuthorityTransfer, error) {
	var out catalog.AuthorityTransfer
	store, err := s.access(ctx, in.Project, "owner")
	if err != nil {
		return out, err
	}
	if managers.Repositories == nil {
		return out, reposource.ErrUnavailable
	}
	authority := s.catalog.Authority(in.Project)
	if (in.To != "github" && in.To != "forgetmenote") || in.To == authority.Type {
		return out, errors.New("choose a different notebook authority")
	}
	source, err := managers.Repositories.Status(in.Project)
	if err != nil {
		return out, err
	}
	if !source.Connected || source.Downloaded == "" || source.Handoff != "" {
		return out, errors.New("connect and refresh the selected GitHub source before previewing a handoff")
	}
	if in.Repository != "" && !strings.EqualFold(in.Repository, source.Repository.FullName) {
		return out, errors.New("the repository must match this notebook's verified Sources App connection")
	}
	local, err := store.NotebookSnapshot(ctx)
	if err != nil {
		return out, err
	}
	remote, err := managers.Repositories.NotebookSnapshot(ctx, in.Project, source.ID, source.Downloaded, source.Repository.DefaultBranch)
	if err != nil {
		return out, err
	}
	if err = workspace.ValidateNotebookSnapshot(ctx, remote); err != nil {
		return out, err
	}
	out = catalog.AuthorityTransfer{To: in.To, Revision: local.Revision, Source: source.ID, SourceGeneration: source.Generation, SourceRevision: source.Downloaded, SourceRepository: source.Repository.ID, SourceBranch: source.Repository.DefaultBranch, Repository: source.Repository.FullName, SnapshotHash: snapshotHash(remote), OperatorWritersStopped: in.OperatorWritersStopped}
	for path, b := range remote.Files {
		out.Files++
		out.Bytes += len(b)
		if strings.HasPrefix(path, "assets/") {
			out.Attachments++
		} else if strings.HasSuffix(path, ".md") && path != "AGENTS.md" {
			out.Notes++
		}
	}
	if in.To == "github" {
		if !in.OperatorWritersStopped {
			return out, errors.New("confirm operator-managed backup writers are stopped before handing this branch to GitHub")
		}
		if managers.Backups == nil {
			return out, errors.New("configure a managed backup and complete its upload before this handoff")
		}
		b, err := managers.Backups.Status(in.Project)
		if err != nil {
			return out, err
		}
		if !b.Connected || b.Repository.ID != source.Repository.ID || b.Branch != source.Repository.DefaultBranch || b.LastRevision != local.Revision || source.Downloaded != local.Revision || snapshotHash(local) != out.SnapshotHash {
			return out, errors.New("the selected private backup's default branch must contain the exact current notebook commit; complete its backup and source refresh first")
		}
		out.BackupGeneration = b.Generation
	}
	return s.catalog.PreviewAuthority(auth.Subject(ctx), in.Project, out)
}
func (s *Authority) Apply(ctx context.Context, in ApplyInput, managers Managers) (catalog.AuthorityTransfer, error) {
	release, err := s.hold(ctx, in.Project)
	if err != nil {
		return catalog.AuthorityTransfer{}, err
	}
	defer release()
	if s.guard(ctx, in.Project, true) != nil {
		return catalog.AuthorityTransfer{}, workspace.ErrForbidden
	}
	p, store, err := s.catalog.BeginAuthority(auth.Subject(ctx), in.Project, in.Preview)
	if err != nil {
		return p, err
	}
	if p.Phase == "complete" {
		return p, nil
	}
	if p.Phase == "publishing" {
		return s.complete(ctx, p, store, managers)
	}
	fail := func(cause error) (catalog.AuthorityTransfer, error) {
		cleanup := s.abort(context.WithoutCancel(ctx), p, store, managers)
		return p, errors.Join(cause, cleanup)
	}
	if managers.Repositories == nil {
		return fail(reposource.ErrUnavailable)
	}
	if _, err = store.PrepareAuthority(ctx, workspace.AuthorityRequest{ID: p.ID, Generation: p.Generation, From: p.From, To: p.To, Revision: p.Revision}); err != nil {
		return fail(err)
	}
	if _, err = managers.Repositories.BeginAuthority(p.Project, p.Actor, p.ID, p.Source, p.SourceGeneration, p.SourceRevision); err != nil {
		return fail(err)
	}
	snapshot, err := managers.Repositories.NotebookSnapshot(ctx, p.Project, p.Source, p.SourceRevision, p.SourceBranch)
	if err != nil {
		return fail(err)
	}
	if snapshotHash(snapshot) != p.SnapshotHash {
		return fail(workspace.ErrConflict)
	}
	if err = workspace.ValidateNotebookSnapshot(ctx, snapshot); err != nil {
		return fail(err)
	}
	if err = managers.Repositories.PinNotebook(ctx, p.Project, p.ID, snapshot); err != nil {
		return fail(err)
	}
	if p.To == "github" {
		if managers.Backups == nil || !p.OperatorWritersStopped {
			return fail(workspace.ErrForbidden)
		}
		binding, err := managers.Backups.BeginAuthority(ctx, p.Project, p.ID, p.BackupGeneration, p.Revision)
		if err != nil {
			return fail(err)
		}
		if !binding.Drained {
			p.Error = "Waiting for the backup worker to stop and acknowledge this handoff. Apply this preview again to continue."
			return p, nil
		}
	} else {
		if _, err = store.StageAuthority(ctx, p.ID, snapshot); err != nil {
			return fail(err)
		}
	}
	// Recheck after worker drain/staging, immediately before publication. The
	// selected upstream commit is pinned; later GitHub edits follow new authority.
	latest, err := managers.Repositories.NotebookSnapshot(ctx, p.Project, p.Source, p.SourceRevision, p.SourceBranch)
	if err != nil {
		return fail(err)
	}
	if snapshotHash(latest) != p.SnapshotHash {
		return fail(workspace.ErrConflict)
	}
	if err = s.check(ctx); err != nil {
		return fail(err)
	}
	// Publication cannot be undone, and FinishAuthority then refuses a
	// notebook no longer at the prepared revision, which would leave the
	// handoff stuck in publishing. Every fenced writer is refused once the
	// handoff is prepared, so only one that is not, such as an earlier
	// release during a rolling deploy, moves the head: the handoff aborts
	// while it still can.
	status, err := store.Status(ctx)
	if err != nil {
		return fail(err)
	}
	if status.Revision != p.Revision {
		return fail(fmt.Errorf("%w: the notebook changed after its handoff was prepared; preview the handoff again", workspace.ErrConflict))
	}
	p, err = s.catalog.PublishAuthority(auth.Subject(ctx), p.Project, p.ID)
	if err != nil {
		return fail(err)
	}
	return s.complete(ctx, p, store, managers)
}
func (s *Authority) complete(ctx context.Context, p catalog.AuthorityTransfer, store *workspace.Store, managers Managers) (catalog.AuthorityTransfer, error) {
	if managers.Repositories == nil {
		return p, reposource.ErrUnavailable
	}
	if err := s.catalog.EnsureAuthorityPublication(p.Project, p.ID); err != nil {
		return p, err
	}
	state, err := store.FinishAuthority(ctx, p.ID, true)
	if err != nil {
		return p, err
	}
	if state.Generation != p.Generation+1 || state.Type != p.To {
		return p, workspace.ErrConflict
	}
	if err = managers.Repositories.FinishAuthority(p.Project, p.ID, p.To, true); err != nil {
		return p, err
	}
	if p.To == "github" {
		if managers.Backups == nil {
			return p, errors.New("restore managed backup configuration to complete the handoff")
		}
		if err = managers.Backups.FinishAuthority(p.Project, p.ID, true); err != nil {
			return p, err
		}
	} else if managers.Backups != nil {
		if err = managers.Backups.ReleaseAuthorityTarget(p.Project); err != nil {
			return p, err
		}
	}
	if err = s.catalog.CompleteAuthority(p.Project, p.ID, state.Committed); err != nil {
		return p, err
	}
	p.Phase = "complete"
	p.ResultRevision = state.Committed
	p.Error = ""
	return p, nil
}
func (s *Authority) abort(ctx context.Context, p catalog.AuthorityTransfer, store *workspace.Store, managers Managers) error {
	// Publication is irreversible here: recovery completes it, never restores a
	// previous tree over content that could already have been accepted.
	if p.Phase == "publishing" || p.Phase == "complete" {
		return workspace.ErrConflict
	}
	if managers.Repositories != nil {
		source, err := managers.Repositories.Status(p.Project)
		if err != nil {
			return err
		}
		if source.Handoff == p.ID {
			if err = managers.Repositories.FinishAuthority(p.Project, p.ID, p.To, false); err != nil {
				return err
			}
		}
	}
	if managers.Backups != nil {
		b, err := managers.Backups.Status(p.Project)
		if err != nil {
			return err
		}
		if b.Handoff == p.ID {
			if err := managers.Backups.FinishAuthority(p.Project, p.ID, false); err != nil {
				return err
			}
		}
	}
	state, err := store.AuthorityStatus(ctx)
	if err != nil {
		return err
	}
	if state.Transfer == p.ID && state.Phase == "prepared" {
		if _, err = store.FinishAuthority(ctx, p.ID, false); err != nil {
			return err
		}
	}
	return s.catalog.AbortAuthority(p.Project, p.ID, "Handoff stopped before publication; original content and authority retained. Backups may require explicit resume.")
}

// Called after provider configuration but before admitting requests. Managers
// persist their own transfer fences, so startup cannot race a late worker. A
// handoff whose lock another gateway holds is that gateway's to finish.
func (s *Authority) Recover(ctx context.Context, managers Managers) error {
	for _, p := range s.catalog.PendingAuthorities() {
		if managers.Repositories == nil || p.BackupGeneration > 0 && managers.Backups == nil {
			return fmt.Errorf("notebook %s: restore Sources/Backups configuration, with the repositories and managed-backups modules on, before authority recovery", p.Project)
		}
		if err := s.recover(ctx, p, managers); err != nil {
			return fmt.Errorf("notebook %s authority recovery: %w", p.Project, err)
		}
	}
	return nil
}
func (s *Authority) recover(ctx context.Context, p catalog.AuthorityTransfer, managers Managers) error {
	release, err := s.hold(ctx, p.Project)
	if errors.Is(err, workspace.ErrConflict) {
		log.Printf("notebook %s: another gateway holds its authority handoff; leaving its recovery to that gateway", p.Project)
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	// The gateway that last held the lock may have finished or advanced the
	// transfer, so recovery acts on it as stored now.
	store, err := s.catalog.AuthorityStore(p.Project, p.ID)
	if errors.Is(err, workspace.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, current := range s.catalog.PendingAuthorities() {
		if current.Project == p.Project && current.ID == p.ID {
			p = current
		}
	}
	if p.Phase == "publishing" {
		_, err = s.complete(ctx, p, store, managers)
	} else {
		err = s.abort(ctx, p, store, managers)
	}
	return err
}

func (s *Authority) Cancel(ctx context.Context, in ApplyInput, managers Managers) (bool, error) {
	if s.guard(ctx, in.Project, false) != nil {
		return false, workspace.ErrForbidden
	}
	release, err := s.hold(ctx, in.Project)
	if err != nil {
		return false, err
	}
	defer release()
	p, err := s.catalog.AuthorityTransfer(auth.Subject(ctx), in.Project)
	if err != nil {
		return false, err
	}
	if p.ID != in.Preview {
		return false, workspace.ErrConflict
	}
	if p.Phase == "preview" || p.Phase == "failed" {
		err = s.catalog.AbortAuthority(p.Project, p.ID, "Canceled")
		return err == nil, err
	}
	store, err := s.catalog.AuthorityStore(p.Project, p.ID)
	if err != nil {
		return false, err
	}
	err = s.abort(ctx, p, store, managers)
	return err == nil, err
}
