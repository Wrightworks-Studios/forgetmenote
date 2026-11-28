package backups

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/drivebackup"
	"forgetmenote/internal/drivebackup/drivetest"
	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

// driveManager opens a manager with fake drive providers and, when github is
// set, the GitHub fixture's App too.
func driveManager(t *testing.T, db *pg.DB, github bool) (*Manager, *drivetest.Fake, *string) {
	t.Helper()
	f := drivetest.New(t)
	check := func(b Binding) error {
		if b.Actor != "alice" || b.Owner != "local" {
			return ErrDenied
		}
		return nil
	}
	var app *githubapp.Client
	head := new(string)
	if github {
		var gm *Manager
		gm, _, head = fixtureOn(t, db)
		app = gm.app
	}
	m, err := Open(db, app, f.Client, check)
	if err != nil {
		t.Fatal(err)
	}
	return m, f, head
}

func connectDrive(t *testing.T, m *Manager, f *drivetest.Fake, kind, project, email string, encrypt bool, generation uint64) (Drive, string, string) {
	t.Helper()
	grant, err := f.Client.Exchange(t.Context(), kind, f.Code(kind, email), "verifier", "https://notes.example.com/backup/callback")
	if err != nil {
		t.Fatal(err)
	}
	d, password, err := m.ConnectDrive(t.Context(), DriveSetup{Drive: Drive{Kind: kind, Project: project, Actor: "alice", Owner: "local", Account: grant.Account, AccountID: grant.AccountID, Path: DrivePath(project), Generation: generation}, Refresh: grant.Refresh, Encrypt: encrypt})
	if err != nil {
		t.Fatal(err)
	}
	return d, password, grant.Refresh
}

// stateHolds reports whether the shared document contains any of secrets.
func stateHolds(t *testing.T, db *pg.DB, secrets ...string) bool {
	t.Helper()
	_, raw, err := db.ReadDocument(t.Context(), db.Pool, stateKind, stateID, -1)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(string(raw), secret) {
			return true
		}
	}
	return false
}

func revealed(value any, secrets ...string) bool {
	raw, _ := json.Marshal(value)
	for _, secret := range secrets {
		if secret != "" && strings.Contains(string(raw), secret) {
			return true
		}
	}
	return false
}

// Without a GitHub App, an Owner's encrypted Google Drive destination leases
// a fresh access token and its password to the worker, records the verified
// snapshot, and on disconnect revokes the grant. No refresh token or password
// reaches the shared state, the worker's list, a status or a receipt.
func TestDriveDestinationLeasesReceiptsAndDisconnects(t *testing.T) {
	db := pgtest.Org(t)
	m, f, _ := driveManager(t, db, false)
	ctx := t.Context()
	if _, err := m.Lease(ctx, "local", "notes", 1, strings.Repeat("a", 40)); !errors.Is(err, ErrDenied) {
		t.Fatal("a manager without a GitHub App leased a GitHub token:", err)
	}
	d, password, refresh := connectDrive(t, m, f, drivebackup.Google, "notes", "alice@example.com", true, 0)
	if !d.Connected || !d.Enabled || !d.Encrypted || d.Generation != 1 || d.Account != "alice@example.com" || !strings.HasPrefix(d.Path, "ForgetMeNote/notes-") || len(password) != 43 {
		t.Fatalf("connected: %+v %q", d, password)
	}
	if stateHolds(t, db, refresh, password) || revealed(d, refresh, password) {
		t.Fatal("a drive credential reached the shared state or the status")
	}
	list, err := m.Worklist("local")
	if err != nil || len(list) != 1 || revealed(list, refresh, password, "sealed") {
		t.Fatalf("worklist: %+v %v", list, err)
	}
	raw, _ := json.Marshal(list[0])
	var entry map[string]any
	json.Unmarshal(raw, &entry)
	if entry["kind"] != "gdrive" || entry["path"] != d.Path || entry["encrypted"] != true || entry["generation"] != float64(1) || entry["enabled"] != true || entry["blocked"] != false {
		t.Fatalf("worker entry: %s", raw)
	}
	sha := strings.Repeat("a", 40)
	if _, err = m.DriveLease(ctx, "another-owner", drivebackup.Google, "notes", d.Generation, sha); !errors.Is(err, ErrDenied) {
		t.Fatal("a worker of another storage owner leased:", err)
	}
	if _, err = m.DriveLease(ctx, "local", drivebackup.Google, "notes", d.Generation+1, sha); !errors.Is(err, ErrDenied) {
		t.Fatal("a lease of another generation was granted:", err)
	}
	if _, err = m.DriveLease(ctx, "local", drivebackup.Dropbox, "notes", d.Generation, sha); !errors.Is(err, ErrDenied) {
		t.Fatal("a lease of an unconnected kind was granted:", err)
	}
	l, err := m.DriveLease(ctx, "local", drivebackup.Google, "notes", d.Generation, sha)
	if err != nil {
		t.Fatal(err)
	}
	if l.Token == "" || l.Path != d.Path || l.Kind != "gdrive" || l.Encryption == nil || l.Encryption.Password != password || time.Until(l.TokenExpires) < minimumTokenLife || time.Until(l.Expires) > 3*time.Minute || time.Until(l.Expires) < 2*time.Minute {
		t.Fatalf("lease: %+v", l)
	}
	if stateHolds(t, db, l.Token, refresh, password) {
		t.Fatal("a leased credential reached the shared state")
	}
	if _, err = m.DriveLease(ctx, "local", drivebackup.Google, "notes", d.Generation, sha); !errors.Is(err, ErrConflict) {
		t.Fatal("a second lease overlapped the first:", err)
	}
	if err = m.CheckLease(ctx, "local", l.ID); err != nil {
		t.Fatal(err)
	}
	receipt := DriveReceipt{Lease: l.ID, Project: "notes", Kind: drivebackup.Google, Generation: l.Generation, Revision: sha, Snapshot: &Snapshot{Name: "notes-20261127T100000Z-aaaaaaaaaaaa.bundle.enc", SHA256: strings.Repeat("b", 64), Size: 1024, Kept: 3, Deleted: []string{"notes-20261101T100000Z-cccccccccccc.bundle.enc"}}}
	wrong := receipt
	wrong.Revision = strings.Repeat("c", 40)
	if _, err = m.CompleteDrive(ctx, "local", wrong); !errors.Is(err, ErrDenied) {
		t.Fatal("a receipt for another commit was accepted:", err)
	}
	empty := receipt
	empty.Snapshot = nil
	if _, err = m.CompleteDrive(ctx, "local", empty); err == nil {
		t.Fatal("a successful receipt without its snapshot was accepted")
	}
	d, err = m.CompleteDrive(ctx, "local", receipt)
	if err != nil || d.LastRevision != sha || d.LastSnapshot != receipt.Snapshot.Name || d.Kept != 3 || d.LastSuccess.IsZero() || d.Error != "" || revealed(d, refresh, password, l.Token) {
		t.Fatalf("receipt: %+v %v", d, err)
	}
	if err = m.CheckLease(ctx, "local", l.ID); !errors.Is(err, ErrDenied) {
		t.Fatal("a completed lease stayed valid:", err)
	}
	if _, err = m.CompleteDrive(ctx, "local", receipt); !errors.Is(err, ErrDenied) {
		t.Fatal("a receipt was accepted twice:", err)
	}
	failed, err := m.DriveLease(ctx, "local", drivebackup.Google, "notes", d.Generation, sha)
	if err != nil {
		t.Fatal(err)
	}
	if d, err = m.CompleteDrive(ctx, "local", DriveReceipt{Lease: failed.ID, Project: "notes", Kind: drivebackup.Google, Generation: d.Generation, Revision: sha, Error: "quota exceeded for alice@example.com"}); err != nil || strings.Contains(d.Error, "alice") || d.Error == "" || d.LastSnapshot != receipt.Snapshot.Name {
		t.Fatalf("failed upload: %+v %v", d, err)
	}
	active, err := m.DriveLease(ctx, "local", drivebackup.Google, "notes", d.Generation, sha)
	if err != nil {
		t.Fatal(err)
	}
	d, err = m.DriveControl(ctx, drivebackup.Google, "notes", d.Generation, "disconnect")
	if err != nil || d.Connected || d.Enabled || d.Error != "" {
		t.Fatalf("disconnect: %+v %v", d, err)
	}
	if f.Live(refresh) {
		t.Fatal("disconnect left the grant live at Google")
	}
	if err = m.CheckLease(ctx, "local", active.ID); !errors.Is(err, ErrDenied) {
		t.Fatal("disconnect left a lease active:", err)
	}
	_, raw, _ = db.ReadDocument(ctx, db.Pool, stateKind, stateID, -1)
	if strings.Contains(string(raw), "sealedRefresh") || strings.Contains(string(raw), "sealedPassword") {
		t.Fatal("a disconnected destination kept its sealed credentials")
	}
	if list, _ := m.Worklist("local"); len(list) != 0 {
		t.Fatal("a disconnected destination is still listed for the worker")
	}
}

// A lease mints its token at the provider, so it fails rather than hand out a
// token about to expire, and a removed grant stops the destination until the
// Owner reconnects.
func TestDriveLeaseRefusesShortTokensAndRevokedGrants(t *testing.T) {
	m, f, _ := driveManager(t, pgtest.Org(t), false)
	ctx := t.Context()
	d, _, _ := connectDrive(t, m, f, drivebackup.Dropbox, "notes", "alice@example.com", false, 0)
	sha := strings.Repeat("a", 40)
	f.Lock()
	f.Lifetime = 600
	f.Unlock()
	if _, err := m.DriveLease(ctx, "local", drivebackup.Dropbox, "notes", d.Generation, sha); !errors.Is(err, ErrUnavailable) {
		t.Fatal("a token with less than ten minutes left was leased:", err)
	}
	f.Lock()
	f.Lifetime = 4 * 3600
	f.Unlock()
	l, err := m.DriveLease(ctx, "local", drivebackup.Dropbox, "notes", d.Generation, sha)
	if err != nil || l.Encryption != nil {
		t.Fatalf("lease: %+v %v", l, err)
	}
	if _, err = m.CompleteDrive(ctx, "local", DriveReceipt{Lease: l.ID, Project: "notes", Kind: drivebackup.Dropbox, Generation: l.Generation, Revision: sha, Error: "network"}); err != nil {
		t.Fatal(err)
	}
	f.RevokeAccount(drivebackup.Dropbox, "alice@example.com")
	if _, err = m.DriveLease(ctx, "local", drivebackup.Dropbox, "notes", d.Generation, sha); !errors.Is(err, ErrDenied) {
		t.Fatal("a revoked grant leased:", err)
	}
	status, err := m.DriveStatus("notes")
	if err != nil || len(status) != 1 || !status[0].Blocked || !strings.Contains(status[0].Error, "reconnect") {
		t.Fatalf("status after revocation: %+v %v", status, err)
	}
	if list, _ := m.Worklist("local"); len(list) != 0 {
		t.Fatal("a blocked destination is listed for the worker")
	}
	// Reconnecting the same account keeps the folder and resumes.
	again, _, _ := connectDrive(t, m, f, drivebackup.Dropbox, "notes", "alice@example.com", true, d.Generation)
	if again.Path != d.Path || again.Encrypted || again.Blocked || !again.Enabled {
		t.Fatalf("reconnect: %+v", again)
	}
}

// A copy restored beside the live instance holds the live instance's grants:
// it neither refreshes nor revokes them, and starts no new destination.
func TestPausedManagerLeavesDriveProvidersAlone(t *testing.T) {
	m, f, _ := driveManager(t, pgtest.Org(t), false)
	ctx := t.Context()
	d, _, refresh := connectDrive(t, m, f, drivebackup.Google, "notes", "alice@example.com", false, 0)
	f.Lock()
	refreshes := f.Refreshes
	f.Unlock()
	m.Outgoing = &outgoing.Mode{}
	m.Outgoing.Pause()
	if _, err := m.DriveLease(ctx, "local", drivebackup.Google, "notes", d.Generation, strings.Repeat("a", 40)); !errors.Is(err, outgoing.ErrPaused) {
		t.Errorf("lease: %v", err)
	}
	for _, action := range []string{"now", "pause", "disconnect"} {
		if _, err := m.DriveControl(ctx, drivebackup.Google, "notes", d.Generation, action); !errors.Is(err, outgoing.ErrPaused) {
			t.Errorf("%s: %v", action, err)
		}
	}
	if _, _, err := m.ConnectDrive(ctx, DriveSetup{Drive: Drive{Kind: drivebackup.Dropbox, Project: "notes", Actor: "alice", Owner: "local", Account: "alice@example.com", AccountID: "x", Path: DrivePath("notes")}, Refresh: "refresh"}); !errors.Is(err, outgoing.ErrPaused) {
		t.Errorf("connect: %v", err)
	}
	f.Lock()
	after, revoked := f.Refreshes, len(f.Revoked)
	f.Unlock()
	if after != refreshes || revoked != 0 || !f.Live(refresh) {
		t.Fatalf("a paused copy reached the provider: %d refreshes, %d revocations", after-refreshes, revoked)
	}
}

// Revoking a Google token ends the client's grant for the whole account, so a
// destination's disconnect keeps a grant another notebook still uses.
func TestDisconnectKeepsAGoogleGrantAnotherNotebookUses(t *testing.T) {
	m, f, _ := driveManager(t, pgtest.Org(t), false)
	ctx := t.Context()
	first, _, firstRefresh := connectDrive(t, m, f, drivebackup.Google, "notes", "alice@example.com", false, 0)
	second, _, secondRefresh := connectDrive(t, m, f, drivebackup.Google, "journal", "alice@example.com", false, 0)
	if first.Path == second.Path {
		t.Fatal("two notebooks share a folder")
	}
	if _, err := m.DriveControl(ctx, drivebackup.Google, "notes", first.Generation, "disconnect"); err != nil {
		t.Fatal(err)
	}
	if !f.Live(secondRefresh) {
		t.Fatal("disconnecting one notebook ended another's Google grant")
	}
	if _, err := m.DriveLease(ctx, "local", drivebackup.Google, "journal", second.Generation, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DriveControl(ctx, drivebackup.Google, "journal", second.Generation, "disconnect"); err != nil {
		t.Fatal(err)
	}
	if f.Live(firstRefresh) || f.Live(secondRefresh) {
		t.Fatal("the last destination's disconnect left the Google grant live")
	}
}

// Connecting another account starts a new destination: a new folder, the
// chosen encryption, and the replaced grant revoked.
func TestReconnectingAnotherAccountStartsANewDestination(t *testing.T) {
	m, f, _ := driveManager(t, pgtest.Org(t), false)
	d, _, old := connectDrive(t, m, f, drivebackup.Dropbox, "notes", "alice@example.com", false, 0)
	next, password, _ := connectDrive(t, m, f, drivebackup.Dropbox, "notes", "bob@example.com", true, d.Generation)
	if next.Path == d.Path || !next.Encrypted || password == "" || next.Account != "bob@example.com" || next.Generation != d.Generation+1 {
		t.Fatalf("new destination: %+v", next)
	}
	if f.Live(old) {
		t.Fatal("the replaced Dropbox grant stayed live")
	}
	if _, _, err := m.ConnectDrive(t.Context(), DriveSetup{Drive: Drive{Kind: drivebackup.Dropbox, Project: "notes", Actor: "alice", Owner: "local", Account: "bob@example.com", AccountID: "dbid:bob@example.com", Path: DrivePath("notes"), Generation: d.Generation}, Refresh: "refresh"}); !errors.Is(err, ErrConflict) {
		t.Fatal("a setup from a stale generation was accepted:", err)
	}
	if _, _, err := m.ConnectDrive(t.Context(), DriveSetup{Drive: Drive{Kind: drivebackup.Google, Project: "notes", Actor: "mallory", Owner: "local", Account: "m@example.com", AccountID: "m@example.com", Path: DrivePath("notes")}, Refresh: "refresh"}); !errors.Is(err, ErrDenied) {
		t.Fatal("a setup by someone the check refuses was accepted:", err)
	}
}

// An authority handoff fences drive destinations as it fences the GitHub
// binding: their generation rises, their leases end, and the handoff waits
// for the worker's drain of each before it can commit, which disconnects them.
func TestHandoffDrainsDriveDestinations(t *testing.T) {
	m, f, head := driveManager(t, pgtest.Org(t), true)
	ctx := t.Context()
	b, err := m.Configure(ctx, Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	*head = sha
	l, err := m.Lease(ctx, "local", "notes", b.Generation, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Complete(ctx, "local", Receipt{Lease: l.ID, Project: "notes", Generation: b.Generation, Revision: sha}); err != nil {
		t.Fatal(err)
	}
	d, _, refresh := connectDrive(t, m, f, drivebackup.Google, "notes", "alice@example.com", true, 0)
	old, err := m.DriveLease(ctx, "local", drivebackup.Google, "notes", d.Generation, sha)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 32)
	b, err = m.BeginAuthority(ctx, "notes", id, b.Generation, sha)
	if err != nil || b.Drained {
		t.Fatal(b, err)
	}
	if _, _, err = m.ConnectDrive(ctx, DriveSetup{Drive: Drive{Kind: drivebackup.Dropbox, Project: "notes", Actor: "alice", Owner: "local", Account: "alice@example.com", AccountID: "x", Path: DrivePath("notes")}, Refresh: "refresh"}); !errors.Is(err, ErrConflict) {
		t.Fatal("a drive destination was connected during a handoff:", err)
	}
	if err = m.CheckLease(ctx, "local", old.ID); !errors.Is(err, ErrDenied) {
		t.Fatal("a drive lease survived the handoff:", err)
	}
	if _, err = m.CompleteDrive(ctx, "local", DriveReceipt{Lease: old.ID, Project: "notes", Kind: drivebackup.Google, Generation: old.Generation, Revision: sha, Snapshot: &Snapshot{Name: "s", SHA256: strings.Repeat("b", 64), Size: 1, Kept: 1}}); !errors.Is(err, ErrDenied) {
		t.Fatal("a receipt from before the handoff was accepted:", err)
	}
	if _, err = m.DriveLease(ctx, "local", drivebackup.Google, "notes", d.Generation+1, sha); !errors.Is(err, ErrDenied) {
		t.Fatal("a drive lease was granted during a handoff:", err)
	}
	status, _ := m.DriveStatus("notes")
	fenced := status[0]
	if fenced.Handoff != id || fenced.Enabled || fenced.Generation != d.Generation+1 {
		t.Fatalf("fenced destination: %+v", fenced)
	}
	list, _ := m.Worklist("local")
	if len(list) != 2 {
		t.Fatalf("the worker does not see both handoffs: %+v", list)
	}
	if entry, ok := list[0].(DriveEntry); !ok || entry.Handoff != id || entry.Generation != fenced.Generation {
		t.Fatalf("drive entries come first, with the handoff: %+v", list[0])
	}
	if err = m.AcknowledgeDrain("local", "notes", id, b.Generation); err != nil {
		t.Fatal(err)
	}
	if again, err := m.BeginAuthority(ctx, "notes", id, b.Generation-1, sha); err != nil || again.Drained {
		t.Fatal("the handoff passed before the drive destination drained", again.Drained, err)
	}
	if err = m.FinishAuthority("notes", id, true); !errors.Is(err, ErrConflict) {
		t.Fatal("the handoff committed before the drive destination drained:", err)
	}
	if err = m.AcknowledgeDriveDrain("local", drivebackup.Google, "notes", id, d.Generation); !errors.Is(err, ErrDenied) {
		t.Fatal("a drain of an older generation was accepted:", err)
	}
	if err = m.AcknowledgeDriveDrain("local", drivebackup.Google, "notes", id, fenced.Generation); err != nil {
		t.Fatal(err)
	}
	if again, err := m.BeginAuthority(ctx, "notes", id, 0, sha); err != nil || !again.Drained {
		t.Fatal("the handoff still waits after every destination drained", again.Drained, err)
	}
	if err = m.FinishAuthority("notes", id, true); err != nil {
		t.Fatal(err)
	}
	status, _ = m.DriveStatus("notes")
	if status[0].Connected || status[0].Handoff != "" || f.Live(refresh) {
		t.Fatalf("a committed handoff left the drive destination connected: %+v", status[0])
	}
}

// An aborted handoff leaves drive destinations paused for an explicit resume.
func TestAbortedHandoffLeavesDriveDestinationsPaused(t *testing.T) {
	m, f, head := driveManager(t, pgtest.Org(t), true)
	ctx := t.Context()
	b, err := m.Configure(ctx, Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"})
	if err != nil {
		t.Fatal(err)
	}
	*head = ""
	connectDrive(t, m, f, drivebackup.Dropbox, "notes", "alice@example.com", false, 0)
	id := strings.Repeat("d", 32)
	if _, err = m.BeginAuthority(ctx, "notes", id, b.Generation, ""); err != nil {
		t.Fatal(err)
	}
	if err = m.FinishAuthority("notes", id, false); err != nil {
		t.Fatal(err)
	}
	status, _ := m.DriveStatus("notes")
	d := status[0]
	if !d.Connected || d.Enabled || d.Handoff != "" {
		t.Fatalf("aborted handoff: %+v", d)
	}
	if d, err = m.DriveControl(ctx, drivebackup.Dropbox, "notes", d.Generation, "resume"); err != nil || !d.Enabled {
		t.Fatal("resume after an aborted handoff:", d, err)
	}
}

// A gateway that predates drive destinations cannot read the state once one
// exists, so it cannot drop them; until then the state stays as it was.
func TestDriveDestinationsRaiseTheStateVersion(t *testing.T) {
	db := pgtest.Org(t)
	m, f, _ := driveManager(t, db, true)
	if _, err := m.Configure(t.Context(), Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"}); err != nil {
		t.Fatal(err)
	}
	version := func() int {
		_, raw, _ := db.ReadDocument(t.Context(), db.Pool, stateKind, stateID, -1)
		var s struct{ Version int }
		json.Unmarshal(raw, &s)
		return s.Version
	}
	if v := version(); v != 1 {
		t.Fatalf("GitHub-only state is version %d", v)
	}
	connectDrive(t, m, f, drivebackup.Google, "notes", "alice@example.com", false, 0)
	if v := version(); v != 3 {
		t.Fatalf("state with drives is version %d", v)
	}
	if err := checkBindings(3, map[string]Binding{}); err == nil {
		t.Fatal("the file-based check accepts version 3")
	}
}
