package workspace

import "context"

// notebookBackend is what a Store serves from its notebook: every method
// below delegates to it. Its one implementation is dbNotebook, the notebook
// kept in the notes store; it is maintained by hand. Operational state stays
// with its concrete owner.
type notebookBackend interface {
	AddComment(ctx context.Context, path, text, revision, author string, options ...CommentOptions) (Discussion, error)
	AddFinding(ctx context.Context, in FindingInput, actor string) (ReviewFinding, error)
	ApplyGroup(ctx context.Context, group EditGroup) (GroupResult, error)
	AssertSnapshot(ctx context.Context, revision string) error
	AuthorityStatus(ctx context.Context) (AuthorityState, error)
	Backup(ctx context.Context, target string) error
	BackupLimited(ctx context.Context, target string, limit int64) error
	BrowseCollection(ctx context.Context, folder, collection string) (FolderBrowse, error)
	BrowseScoped(ctx context.Context, folder string, scope ContentScope) (FolderBrowse, error)
	CaptureAbortReceive(ctx context.Context, in CaptureMove) (CaptureReceipt, error)
	CaptureCancel(ctx context.Context, in CaptureMove) (CaptureReceipt, error)
	CaptureCreate(ctx context.Context, id, path string, content []byte) (Document, error)
	CaptureFinish(ctx context.Context, in CaptureMove) (CaptureReceipt, error)
	CapturePreflight(ctx context.Context, in CaptureMove) (CaptureReceipt, error)
	CapturePrepare(ctx context.Context, in CaptureMove) (Document, error)
	CaptureReceive(ctx context.Context, in CaptureMove, content []byte) (Document, error)
	CaptureSaved(ctx context.Context, id, path, hash string) (Document, error)
	ChangesSince(ctx context.Context, from, revision string) (ChangeSet, error)
	Close() error
	Collections(ctx context.Context) (Collections, error)
	Comments(ctx context.Context, path string) (Discussion, error)
	DecideFinding(ctx context.Context, path, id, revision, actor, decision string, canEdit bool) (ReviewFinding, error)
	DeleteComment(ctx context.Context, path, id, replyID, revision, author string, canModerate bool) (Discussion, error)
	Duplicate(ctx context.Context, from, to, expected, actor string) (MoveResult, error)
	EmptyTrash(ctx context.Context, expected, confirmation string) (TrashResult, error)
	Find(ctx context.Context, query, tag, status, pathPrefix string) ([]Entry, error)
	FindScoped(ctx context.Context, query, tag, status, path string, includeTrash bool, limit int, scope ContentScope) (ScopedEntries, error)
	Findings(ctx context.Context) ([]ReviewFinding, error)
	FinishAuthority(ctx context.Context, id string, commit bool) (AuthorityState, error)
	GroupReceipt(ctx context.Context, group EditGroup, identityID string) (GroupResult, error)
	History(ctx context.Context, path string) ([]Commit, error)
	HistoryNotes(ctx context.Context, revision string, offset int, known map[string]SnapshotFile) (HistoryBatch, error)
	HistoryRevisions(ctx context.Context, target, after string) ([]HistoryRevision, error)
	HistoryScoped(ctx context.Context, path string, scope ContentScope) ([]Commit, error)
	HistoryVisibilityID(ctx context.Context) (string, error)
	ImportSource(ctx context.Context, capture SourceCapture, target, expected, title string, folder *string) (Document, error)
	KnowledgeRead(ctx context.Context, path, revision string) (Document, error)
	List(ctx context.Context) ([]Entry, error)
	ListCollection(ctx context.Context, collection string) ([]Entry, error)
	ListTrash(ctx context.Context) (TrashListing, error)
	LiveApply(ctx context.Context, in LiveUpdate) (LiveDocument, error)
	LiveCheckpoint(ctx context.Context, path string) (LiveDocument, error)
	LivePatch(ctx context.Context, in TargetedPatch) (LiveDocument, error)
	LiveRead(ctx context.Context, path string) (LiveDocument, error)
	Locks(ctx context.Context, path string) (LockState, error)
	Move(ctx context.Context, from, to, expected, actor string) (MoveResult, error)
	NotebookSnapshot(ctx context.Context) (NotebookSnapshot, error)
	OrganizationReceipt(ctx context.Context, a OrganizationAction, actor string) (OrganizationResult, error)
	OrganizationSources(ctx context.Context, sources []MutationSource) error
	Organize(ctx context.Context, a OrganizationAction, apply bool) (OrganizationResult, error)
	PrepareAuthority(ctx context.Context, in AuthorityRequest) (AuthorityState, error)
	Read(ctx context.Context, path, revision string) (Document, error)
	ResolveComment(ctx context.Context, path, id, revision, author string, canEdit bool) (Discussion, error)
	RestoreTrash(ctx context.Context, id, to, expected, actor string) (TrashResult, error)
	Save(ctx context.Context, path string, content []byte, expected, actor string) (Document, error)
	SetContainer(ctx context.Context, path, container, expected, actor string) (Document, error)
	SetLock(ctx context.Context, path string, locked bool, expected, actor string) (LockState, error)
	SetMetadata(ctx context.Context, path string, m Metadata, expected, actor string) (Document, error)
	Snapshot(ctx context.Context, revision string) (Snapshot, error)
	SnapshotScoped(ctx context.Context, revision string, scope ContentScope) (Snapshot, error)
	StageAuthority(ctx context.Context, id string, snapshot NotebookSnapshot) (AuthorityState, error)
	Status(ctx context.Context) (Status, error)
	TaskHistory(ctx context.Context, task string) ([]Commit, error)
	TrashNotes(ctx context.Context, paths []string, expected, actor string) (TrashResult, error)
	TrashPolicy(ctx context.Context) (TrashIndex, error)
	UndoOrganization(ctx context.Context, input OrganizationUndo) (OrganizationResult, error)
	ValidateMutationGroup(ctx context.Context, group EditGroup) error
	VerifySource(ctx context.Context, target, revision string) (SourceVerification, error)
	VisibleHistoryPaths(ctx context.Context, paths []string) (map[string]bool, error)
	WaitLive(ctx context.Context, path, known string, milliseconds int, incremental bool) (LiveDocument, error)
	Watch() (<-chan struct{}, func(), error)
	WholeHistory(ctx context.Context, path, revision string) ([]Commit, error)
	WriteInstructions(ctx context.Context, content []byte, expected, actor string) (Document, error)
}

func (s *Store) AddComment(ctx context.Context, path, text, revision, author string, options ...CommentOptions) (Discussion, error) {
	return s.backend.AddComment(ctx, path, text, revision, author, options...)
}

func (s *Store) AddFinding(ctx context.Context, in FindingInput, actor string) (ReviewFinding, error) {
	return s.backend.AddFinding(ctx, in, actor)
}

func (s *Store) ApplyGroup(ctx context.Context, group EditGroup) (GroupResult, error) {
	return s.backend.ApplyGroup(ctx, group)
}

func (s *Store) AssertSnapshot(ctx context.Context, revision string) error {
	return s.backend.AssertSnapshot(ctx, revision)
}

func (s *Store) AuthorityStatus(ctx context.Context) (AuthorityState, error) {
	return s.backend.AuthorityStatus(ctx)
}

func (s *Store) Backup(ctx context.Context, target string) error {
	return s.backend.Backup(ctx, target)
}

func (s *Store) BackupLimited(ctx context.Context, target string, limit int64) error {
	return s.backend.BackupLimited(ctx, target, limit)
}

func (s *Store) BrowseCollection(ctx context.Context, folder, collection string) (FolderBrowse, error) {
	return s.backend.BrowseCollection(ctx, folder, collection)
}

func (s *Store) BrowseScoped(ctx context.Context, folder string, scope ContentScope) (FolderBrowse, error) {
	return s.backend.BrowseScoped(ctx, folder, scope)
}

func (s *Store) CaptureAbortReceive(ctx context.Context, in CaptureMove) (CaptureReceipt, error) {
	return s.backend.CaptureAbortReceive(ctx, in)
}

func (s *Store) CaptureCancel(ctx context.Context, in CaptureMove) (CaptureReceipt, error) {
	return s.backend.CaptureCancel(ctx, in)
}

func (s *Store) CaptureCreate(ctx context.Context, id, path string, content []byte) (Document, error) {
	return s.backend.CaptureCreate(ctx, id, path, content)
}

func (s *Store) CaptureFinish(ctx context.Context, in CaptureMove) (CaptureReceipt, error) {
	return s.backend.CaptureFinish(ctx, in)
}

func (s *Store) CapturePreflight(ctx context.Context, in CaptureMove) (CaptureReceipt, error) {
	return s.backend.CapturePreflight(ctx, in)
}

func (s *Store) CapturePrepare(ctx context.Context, in CaptureMove) (Document, error) {
	return s.backend.CapturePrepare(ctx, in)
}

func (s *Store) CaptureReceive(ctx context.Context, in CaptureMove, content []byte) (Document, error) {
	return s.backend.CaptureReceive(ctx, in, content)
}

func (s *Store) CaptureSaved(ctx context.Context, id, path, hash string) (Document, error) {
	return s.backend.CaptureSaved(ctx, id, path, hash)
}

func (s *Store) ChangesSince(ctx context.Context, from, revision string) (ChangeSet, error) {
	return s.backend.ChangesSince(ctx, from, revision)
}

func (s *Store) Close() error { return s.backend.Close() }

func (s *Store) Collections(ctx context.Context) (Collections, error) {
	return s.backend.Collections(ctx)
}

func (s *Store) Comments(ctx context.Context, path string) (Discussion, error) {
	return s.backend.Comments(ctx, path)
}

func (s *Store) DecideFinding(ctx context.Context, path, id, revision, actor, decision string, canEdit bool) (ReviewFinding, error) {
	return s.backend.DecideFinding(ctx, path, id, revision, actor, decision, canEdit)
}

func (s *Store) DeleteComment(ctx context.Context, path, id, replyID, revision, author string, canModerate bool) (Discussion, error) {
	return s.backend.DeleteComment(ctx, path, id, replyID, revision, author, canModerate)
}

func (s *Store) Duplicate(ctx context.Context, from, to, expected, actor string) (MoveResult, error) {
	return s.backend.Duplicate(ctx, from, to, expected, actor)
}

func (s *Store) EmptyTrash(ctx context.Context, expected, confirmation string) (TrashResult, error) {
	return s.backend.EmptyTrash(ctx, expected, confirmation)
}

func (s *Store) Find(ctx context.Context, query, tag, status, pathPrefix string) ([]Entry, error) {
	return s.backend.Find(ctx, query, tag, status, pathPrefix)
}

func (s *Store) FindScoped(ctx context.Context, query, tag, status, path string, includeTrash bool, limit int, scope ContentScope) (ScopedEntries, error) {
	return s.backend.FindScoped(ctx, query, tag, status, path, includeTrash, limit, scope)
}

func (s *Store) Findings(ctx context.Context) ([]ReviewFinding, error) {
	return s.backend.Findings(ctx)
}

func (s *Store) FinishAuthority(ctx context.Context, id string, commit bool) (AuthorityState, error) {
	return s.backend.FinishAuthority(ctx, id, commit)
}

func (s *Store) GroupReceipt(ctx context.Context, group EditGroup, identityID string) (GroupResult, error) {
	return s.backend.GroupReceipt(ctx, group, identityID)
}

func (s *Store) History(ctx context.Context, path string) ([]Commit, error) {
	return s.backend.History(ctx, path)
}

func (s *Store) HistoryNotes(ctx context.Context, revision string, offset int, known map[string]SnapshotFile) (HistoryBatch, error) {
	return s.backend.HistoryNotes(ctx, revision, offset, known)
}

func (s *Store) HistoryRevisions(ctx context.Context, target, after string) ([]HistoryRevision, error) {
	return s.backend.HistoryRevisions(ctx, target, after)
}

func (s *Store) HistoryScoped(ctx context.Context, path string, scope ContentScope) ([]Commit, error) {
	return s.backend.HistoryScoped(ctx, path, scope)
}

func (s *Store) HistoryVisibilityID(ctx context.Context) (string, error) {
	return s.backend.HistoryVisibilityID(ctx)
}

func (s *Store) ImportSource(ctx context.Context, capture SourceCapture, target, expected, title string, folder *string) (Document, error) {
	return s.backend.ImportSource(ctx, capture, target, expected, title, folder)
}

func (s *Store) KnowledgeRead(ctx context.Context, path, revision string) (Document, error) {
	return s.backend.KnowledgeRead(ctx, path, revision)
}

func (s *Store) List(ctx context.Context) ([]Entry, error) { return s.backend.List(ctx) }

func (s *Store) ListCollection(ctx context.Context, collection string) ([]Entry, error) {
	return s.backend.ListCollection(ctx, collection)
}

func (s *Store) ListTrash(ctx context.Context) (TrashListing, error) { return s.backend.ListTrash(ctx) }

func (s *Store) LiveApply(ctx context.Context, in LiveUpdate) (LiveDocument, error) {
	return s.backend.LiveApply(ctx, in)
}

func (s *Store) LiveCheckpoint(ctx context.Context, path string) (LiveDocument, error) {
	return s.backend.LiveCheckpoint(ctx, path)
}

func (s *Store) LivePatch(ctx context.Context, in TargetedPatch) (LiveDocument, error) {
	return s.backend.LivePatch(ctx, in)
}

func (s *Store) LiveRead(ctx context.Context, path string) (LiveDocument, error) {
	return s.backend.LiveRead(ctx, path)
}

func (s *Store) Locks(ctx context.Context, path string) (LockState, error) {
	return s.backend.Locks(ctx, path)
}

func (s *Store) Move(ctx context.Context, from, to, expected, actor string) (MoveResult, error) {
	return s.backend.Move(ctx, from, to, expected, actor)
}

func (s *Store) NotebookSnapshot(ctx context.Context) (NotebookSnapshot, error) {
	return s.backend.NotebookSnapshot(ctx)
}

func (s *Store) OrganizationReceipt(ctx context.Context, a OrganizationAction, actor string) (OrganizationResult, error) {
	return s.backend.OrganizationReceipt(ctx, a, actor)
}

func (s *Store) OrganizationSources(ctx context.Context, sources []MutationSource) error {
	return s.backend.OrganizationSources(ctx, sources)
}

func (s *Store) Organize(ctx context.Context, a OrganizationAction, apply bool) (OrganizationResult, error) {
	return s.backend.Organize(ctx, a, apply)
}

func (s *Store) PrepareAuthority(ctx context.Context, in AuthorityRequest) (AuthorityState, error) {
	return s.backend.PrepareAuthority(ctx, in)
}

func (s *Store) Read(ctx context.Context, path, revision string) (Document, error) {
	return s.backend.Read(ctx, path, revision)
}

func (s *Store) ResolveComment(ctx context.Context, path, id, revision, author string, canEdit bool) (Discussion, error) {
	return s.backend.ResolveComment(ctx, path, id, revision, author, canEdit)
}

func (s *Store) RestoreTrash(ctx context.Context, id, to, expected, actor string) (TrashResult, error) {
	return s.backend.RestoreTrash(ctx, id, to, expected, actor)
}

func (s *Store) Save(ctx context.Context, path string, content []byte, expected, actor string) (Document, error) {
	return s.backend.Save(ctx, path, content, expected, actor)
}

func (s *Store) SetContainer(ctx context.Context, path, container, expected, actor string) (Document, error) {
	return s.backend.SetContainer(ctx, path, container, expected, actor)
}

func (s *Store) SetLock(ctx context.Context, path string, locked bool, expected, actor string) (LockState, error) {
	return s.backend.SetLock(ctx, path, locked, expected, actor)
}

func (s *Store) SetMetadata(ctx context.Context, path string, m Metadata, expected, actor string) (Document, error) {
	return s.backend.SetMetadata(ctx, path, m, expected, actor)
}

func (s *Store) Snapshot(ctx context.Context, revision string) (Snapshot, error) {
	return s.backend.Snapshot(ctx, revision)
}

func (s *Store) SnapshotScoped(ctx context.Context, revision string, scope ContentScope) (Snapshot, error) {
	return s.backend.SnapshotScoped(ctx, revision, scope)
}

func (s *Store) StageAuthority(ctx context.Context, id string, snapshot NotebookSnapshot) (AuthorityState, error) {
	return s.backend.StageAuthority(ctx, id, snapshot)
}

func (s *Store) Status(ctx context.Context) (Status, error) { return s.backend.Status(ctx) }

func (s *Store) TaskHistory(ctx context.Context, task string) ([]Commit, error) {
	return s.backend.TaskHistory(ctx, task)
}

func (s *Store) TrashNotes(ctx context.Context, paths []string, expected, actor string) (TrashResult, error) {
	return s.backend.TrashNotes(ctx, paths, expected, actor)
}

func (s *Store) TrashPolicy(ctx context.Context) (TrashIndex, error) {
	return s.backend.TrashPolicy(ctx)
}

func (s *Store) UndoOrganization(ctx context.Context, input OrganizationUndo) (OrganizationResult, error) {
	return s.backend.UndoOrganization(ctx, input)
}

func (s *Store) ValidateMutationGroup(ctx context.Context, group EditGroup) error {
	return s.backend.ValidateMutationGroup(ctx, group)
}

func (s *Store) VerifySource(ctx context.Context, target, revision string) (SourceVerification, error) {
	return s.backend.VerifySource(ctx, target, revision)
}

func (s *Store) VisibleHistoryPaths(ctx context.Context, paths []string) (map[string]bool, error) {
	return s.backend.VisibleHistoryPaths(ctx, paths)
}

func (s *Store) WaitLive(ctx context.Context, path, known string, milliseconds int, incremental bool) (LiveDocument, error) {
	return s.backend.WaitLive(ctx, path, known, milliseconds, incremental)
}

func (s *Store) Watch() (<-chan struct{}, func(), error) { return s.backend.Watch() }

func (s *Store) WholeHistory(ctx context.Context, path, revision string) ([]Commit, error) {
	return s.backend.WholeHistory(ctx, path, revision)
}

func (s *Store) WriteInstructions(ctx context.Context, content []byte, expected, actor string) (Document, error) {
	return s.backend.WriteInstructions(ctx, content, expected, actor)
}
