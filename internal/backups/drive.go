package backups

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"sort"
	"time"

	"forgetmenote/internal/drivebackup"
)

// A Drive is a notebook's Google Drive or Dropbox destination. The worker
// copies snapshots of the notebook's Git export into Path, a folder the app
// chose at connect time, under the provider account Account. The refresh
// token and any encryption password stay sealed in storedDrive and never
// appear here.
type Drive struct {
	Kind               string    `json:"kind"`
	Project            string    `json:"project"`
	Actor              string    `json:"actor"`
	Owner              string    `json:"owner"`
	Epoch              uint64    `json:"epoch"`
	Authority          uint64    `json:"authority"`
	NotebookGeneration uint64    `json:"notebookGeneration,omitempty"`
	Account            string    `json:"account,omitempty"`
	AccountID          string    `json:"accountId,omitempty"`
	Path               string    `json:"path,omitempty"`
	Encrypted          bool      `json:"encrypted"`
	Generation         uint64    `json:"generation"`
	Connected          bool      `json:"connected"`
	Enabled            bool      `json:"enabled"`
	Blocked            bool      `json:"blocked,omitempty"`
	Request            uint64    `json:"request"`
	Handoff            string    `json:"handoff,omitempty"`
	Drained            bool      `json:"drained,omitempty"`
	LastRevision       string    `json:"lastRevision,omitempty"`
	LastSnapshot       string    `json:"lastSnapshot,omitempty"`
	LastSHA256         string    `json:"lastSha256,omitempty"`
	LastSize           int64     `json:"lastSize,omitempty"`
	Kept               int       `json:"kept,omitempty"`
	LastSuccess        time.Time `json:"lastSuccess,omitzero"`
	LastAttempt        time.Time `json:"lastAttempt,omitzero"`
	Error              string    `json:"error,omitempty"`
}

// storedDrive is a destination as the shared state keeps it, with its
// refresh token and encryption password sealed under the drive seal key,
// which the database does not hold.
type storedDrive struct {
	Drive
	Refresh  string `json:"sealedRefresh,omitempty"`
	Password string `json:"sealedPassword,omitempty"`
}

// DriveSetup connects a destination from a grant the Owner just authorized.
// Generation is the destination's generation the Owner saw; Path is the
// folder offered for a new destination.
type DriveSetup struct {
	Drive
	Refresh string
	Encrypt bool
}

// DriveEntry is a destination as the worker sees it.
type DriveEntry struct {
	Kind               string `json:"kind"`
	Project            string `json:"project"`
	Generation         uint64 `json:"generation"`
	NotebookGeneration uint64 `json:"notebookGeneration"`
	Path               string `json:"path"`
	Encrypted          bool   `json:"encrypted"`
	Enabled            bool   `json:"enabled"`
	Blocked            bool   `json:"blocked"`
	Request            uint64 `json:"request"`
	Handoff            string `json:"handoff,omitempty"`
	LastRevision       string `json:"lastRevision,omitempty"`
	LastSnapshot       string `json:"lastSnapshot,omitempty"`
}

// DriveLease is one upload's access: an access token minted for it with at
// least ten minutes left, and the encryption password of an encrypted
// destination.
type DriveLease struct {
	ID                 string      `json:"id"`
	Project            string      `json:"project"`
	Kind               string      `json:"kind"`
	Generation         uint64      `json:"generation"`
	NotebookGeneration uint64      `json:"notebookGeneration"`
	Revision           string      `json:"revision"`
	Path               string      `json:"path"`
	Token              string      `json:"token"`
	TokenExpires       time.Time   `json:"tokenExpires"`
	Expires            time.Time   `json:"expires"`
	Encryption         *Encryption `json:"encryption"`
}
type Encryption struct {
	Password string `json:"password"`
}

// DriveReceipt is a worker's report of one drive upload.
type DriveReceipt struct {
	Lease      string    `json:"lease"`
	Project    string    `json:"project"`
	Kind       string    `json:"kind"`
	Generation uint64    `json:"generation"`
	Revision   string    `json:"revision"`
	Error      string    `json:"error,omitempty"`
	Diverged   bool      `json:"diverged,omitempty"`
	Snapshot   *Snapshot `json:"snapshot,omitempty"`
}

// Snapshot is the file an upload verified at the provider, the number of
// snapshots the folder keeps after it, and those retention deleted.
type Snapshot struct {
	Name    string   `json:"name"`
	SHA256  string   `json:"sha256"`
	Size    int64    `json:"size"`
	Kept    int      `json:"kept"`
	Deleted []string `json:"deleted"`
}

// minimumTokenLife is the least lifetime a leased access token has left, so an
// upload that starts within its three-minute lease can finish.
const minimumTokenLife = 10 * time.Minute

var drivePath = regexp.MustCompile(`^ForgetMeNote/[a-z0-9][a-z0-9-]{0,63}-[a-z0-9]{6}$`)
var snapshotName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)
var sha256Hex = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validKind(kind string) bool { return slices.Contains(drivebackup.Kinds, kind) }
func driveKey(kind, project string) string {
	return kind + ":" + project
}
func refreshPurpose(key string) string  { return "refresh " + key }
func passwordPurpose(key string) string { return "password " + key }

// DrivePath is a new destination folder for project:
// ForgetMeNote/<project>-<six random lowercase letters and digits>.
func DrivePath(project string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	suffix := make([]byte, 6)
	for i := range suffix {
		var b [1]byte
		for {
			rand.Read(b[:])
			// Reject the bias of 256 % 36.
			if int(b[0]) < 252 {
				break
			}
		}
		suffix[i] = alphabet[int(b[0])%len(alphabet)]
	}
	return "ForgetMeNote/" + project + "-" + string(suffix)
}

func checkDrives(drives map[string]storedDrive) error {
	folders := map[string]bool{}
	for key, d := range drives {
		if key != driveKey(d.Kind, d.Project) || !validKind(d.Kind) || !identifier.MatchString(d.Project) || d.Owner == "" || d.Actor == "" {
			return errors.New("damaged drive backup destination")
		}
		if d.Connected && (!drivePath.MatchString(d.Path) || d.Generation == 0 || d.Refresh == "" || d.Account == "" || d.Encrypted && d.Password == "") {
			return errors.New("damaged drive backup destination")
		}
		if d.Connected && folders[d.Kind+":"+d.Path] {
			return errors.New("drive backup folder is bound twice")
		}
		if d.Connected {
			folders[d.Kind+":"+d.Path] = true
		}
	}
	return nil
}

func sortDrives(drives []Drive) {
	sort.Slice(drives, func(i, j int) bool {
		if drives[i].Project != drives[j].Project {
			return drives[i].Project < drives[j].Project
		}
		return slices.Index(drivebackup.Kinds, drives[i].Kind) < slices.Index(drivebackup.Kinds, drives[j].Kind)
	})
}

// binding is the authorization the check function verifies for a drive.
func (d Drive) binding() Binding {
	return Binding{Project: d.Project, Actor: d.Actor, Owner: d.Owner, Epoch: d.Epoch, Authority: d.Authority, NotebookGeneration: d.NotebookGeneration}
}

// Drives reports whether this manager serves kind's destinations.
func (m *Manager) Drives(kind string) bool { return m.drives.Configured(kind) }

// DriveKinds lists the configured drive kinds.
func (m *Manager) DriveKinds() []string { return m.drives.Kinds() }

func (m *Manager) driveAllowed(d Drive, owner string, generation uint64) bool {
	return m.drives.Configured(d.Kind) && d.Owner == owner && d.Connected && d.Enabled && !d.Blocked && d.Handoff == "" && d.Generation == generation && m.check(d.binding()) == nil
}

// DriveStatus reports a notebook's drive destinations as every gateway
// currently shares them, in Kinds order.
func (m *Manager) DriveStatus(project string) ([]Drive, error) {
	if err := m.lock(); err != nil {
		return nil, err
	}
	defer m.mu.Unlock()
	out := []Drive{}
	for _, kind := range drivebackup.Kinds {
		if d, ok := m.state.Drives[driveKey(kind, project)]; ok {
			out = append(out, m.status(d.Drive))
		}
	}
	return out, nil
}

func (m *Manager) status(d Drive) Drive {
	if d.Connected && m.check(d.binding()) != nil {
		d.Error = "Backup authorization changed; reconnect as a current notebook Owner"
	}
	return d
}

// Overview reports every notebook's GitHub binding and drive destinations, for
// an Admin. A binding's authorization is reported, not enforced.
func (m *Manager) Overview() (map[string]Binding, map[string][]Drive, error) {
	if err := m.lock(); err != nil {
		return nil, nil, err
	}
	defer m.mu.Unlock()
	bindings := map[string]Binding{}
	for p, b := range m.state.Bindings {
		if b.Connected && m.check(b) != nil {
			b.Error = "Backup authorization changed; reconnect as a current notebook Owner"
		}
		b.Creation = nil
		bindings[p] = b
	}
	drives := map[string][]Drive{}
	for _, d := range m.state.Drives {
		drives[d.Project] = append(drives[d.Project], m.status(d.Drive))
	}
	for _, list := range drives {
		sortDrives(list)
	}
	return bindings, drives, nil
}

// ConnectDrive connects or reconnects a drive destination. Reconnecting the
// same provider account keeps the destination's folder, encryption and
// receipts; another account starts a new destination in the offered folder.
// A new encrypted destination returns its password, the only time it leaves
// the shared state except in a lease.
func (m *Manager) ConnectDrive(ctx context.Context, in DriveSetup) (Drive, string, error) {
	if err := m.Outgoing.Check(); err != nil {
		return Drive{}, "", err
	}
	if !m.drives.Configured(in.Kind) {
		return Drive{}, "", ErrUnavailable
	}
	if err := m.lock(); err != nil {
		return Drive{}, "", err
	}
	defer m.mu.Unlock()
	key := driveKey(in.Kind, in.Project)
	old := m.state.Drives[key]
	if old.Generation != in.Generation || old.Handoff != "" || m.state.Bindings[in.Project].Handoff != "" || m.state.Bindings[in.Project].AuthorityTarget {
		return Drive{}, "", ErrConflict
	}
	if !identifier.MatchString(in.Project) || in.Owner == "" || in.Actor == "" || in.Refresh == "" || in.Account == "" || in.AccountID == "" {
		return Drive{}, "", ErrDenied
	}
	if err := m.check(in.binding()); err != nil {
		return Drive{}, "", err
	}
	continuing := old.Connected && old.AccountID == in.AccountID
	d := storedDrive{Drive: Drive{Kind: in.Kind, Project: in.Project, Actor: in.Actor, Owner: in.Owner, Epoch: in.Epoch, Authority: in.Authority, NotebookGeneration: in.NotebookGeneration, Account: in.Account, AccountID: in.AccountID}}
	if continuing {
		d.Path, d.Encrypted, d.Password = old.Path, old.Encrypted, old.Password
		d.LastRevision, d.LastSnapshot, d.LastSHA256, d.LastSize, d.Kept, d.LastSuccess = old.LastRevision, old.LastSnapshot, old.LastSHA256, old.LastSize, old.Kept, old.LastSuccess
	} else {
		if !drivePath.MatchString(in.Path) {
			return Drive{}, "", ErrDenied
		}
		d.Path, d.Encrypted = in.Path, in.Encrypt
	}
	for other, existing := range m.state.Drives {
		if other != key && existing.Connected && existing.Kind == d.Kind && existing.Path == d.Path {
			return Drive{}, "", errors.New("another notebook already uses this backup folder")
		}
	}
	password := ""
	var err error
	if d.Encrypted && !continuing {
		password = drivebackup.Password()
		if d.Password, err = m.drives.Seal(passwordPurpose(key), password); err != nil {
			return Drive{}, "", err
		}
	}
	if d.Refresh, err = m.drives.Seal(refreshPurpose(key), in.Refresh); err != nil {
		return Drive{}, "", err
	}
	d.Connected, d.Enabled = true, true
	d.Generation = old.Generation + 1
	d.Request = old.Request + 1
	// The replaced grant ends unless the new one is the same Google grant or
	// another notebook's destination shares it.
	replaced := ""
	if old.Connected && old.Refresh != "" && !(continuing && d.Kind == drivebackup.Google) && !m.sharedGrant(old.Drive, key) {
		if previous, e := m.drives.Open(refreshPurpose(key), old.Refresh); e == nil && previous != in.Refresh {
			replaced = previous
		}
	}
	leases, tokens := m.without(func(l Lease) bool { return l.Project == d.Project && l.Kind == d.Kind })
	if err = m.commit(leases, nil, []storedDrive{d}); err != nil {
		return Drive{}, "", err
	}
	m.revoke(ctx, tokens...)
	if replaced != "" {
		_ = m.revokeDrive(ctx, d.Kind, replaced)
	}
	return d.Drive, password, nil
}

// sharedGrant reports whether another connected destination holds the same
// Google grant: revoking a Google token ends that client's grant for the
// whole account, so it stays while another destination uses it.
func (m *Manager) sharedGrant(d Drive, except string) bool {
	if d.Kind != drivebackup.Google {
		return false
	}
	for key, other := range m.state.Drives {
		if key != except && other.Connected && other.Kind == drivebackup.Google && other.AccountID == d.AccountID {
			return true
		}
	}
	return false
}

// revokeDrive ends a grant at the provider. A paused copy's grants are the live
// instance's, so it leaves them alone.
func (m *Manager) revokeDrive(ctx context.Context, kind, refresh string) error {
	if m.Outgoing.Paused() || refresh == "" {
		return nil
	}
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return m.drives.Revoke(revokeCtx, kind, refresh)
}

// AbandonGrant revokes a grant that a setup obtained and will not use, unless
// it is the Google grant of an account a connected destination uses.
func (m *Manager) AbandonGrant(ctx context.Context, kind, accountID, refresh string) error {
	if err := m.lock(); err != nil {
		return err
	}
	shared := m.sharedGrant(Drive{Kind: kind, AccountID: accountID}, "")
	m.mu.Unlock()
	if shared || !m.drives.Configured(kind) {
		return nil
	}
	return m.revokeDrive(ctx, kind, refresh)
}

// DriveControl pauses, resumes, requests an upload from, or disconnects a
// drive destination. Disconnecting revokes the grant at the provider and
// forgets its credentials; uploaded snapshots stay in the folder.
func (m *Manager) DriveControl(ctx context.Context, kind, project string, generation uint64, action string) (Drive, error) {
	if err := m.Outgoing.Check(); err != nil {
		return Drive{}, err
	}
	if err := m.lock(); err != nil {
		return Drive{}, err
	}
	defer m.mu.Unlock()
	key := driveKey(kind, project)
	d, ok := m.state.Drives[key]
	if !ok || !d.Connected || d.Generation != generation || d.Handoff != "" {
		return d.Drive, ErrConflict
	}
	refresh := ""
	switch action {
	case "pause":
		d.Enabled = false
	case "resume":
		if err := m.check(d.binding()); err != nil {
			return d.Drive, err
		}
		if !m.drives.Configured(kind) {
			return d.Drive, ErrUnavailable
		}
		d.Enabled, d.Blocked, d.Error = true, false, ""
	case "now":
		if err := m.check(d.binding()); err != nil {
			return d.Drive, err
		}
		if !d.Enabled || d.Blocked {
			return d.Drive, errors.New("resume backups before requesting an upload")
		}
		d.Request++
		return d.Drive, m.commit(m.leases, nil, []storedDrive{d})
	case "disconnect":
		if !m.sharedGrant(d.Drive, key) && m.drives.Configured(kind) {
			refresh, _ = m.drives.Open(refreshPurpose(key), d.Refresh)
		}
		d.Enabled, d.Connected, d.Blocked, d.Error = false, false, false, ""
		d.Refresh, d.Password = "", ""
	default:
		return d.Drive, ErrDenied
	}
	d.Generation++
	leases, tokens := m.without(func(l Lease) bool { return l.Project == project && l.Kind == kind })
	if err := m.commit(leases, nil, []storedDrive{d}); err != nil {
		return d.Drive, err
	}
	m.revoke(ctx, tokens...)
	if refresh != "" {
		if err := m.revokeDrive(ctx, kind, refresh); err != nil {
			d.Error = "Disconnected, but " + drivebackup.Name(kind) + " did not confirm removing access. Remove ForgetMeNote from the account's connected apps."
			_ = m.commit(m.leases, nil, []storedDrive{d})
		}
	}
	return d.Drive, nil
}

// DriveLease grants a worker one upload to a drive destination: it refreshes
// an access token for it and, for an encrypted destination, opens the
// password. At most one lease per destination is active.
func (m *Manager) DriveLease(ctx context.Context, owner, kind, project string, generation uint64, sha string) (DriveLease, error) {
	if err := m.Outgoing.Check(); err != nil {
		return DriveLease{}, err
	}
	if !m.drives.Configured(kind) {
		return DriveLease{}, ErrDenied
	}
	if err := m.lock(); err != nil {
		return DriveLease{}, err
	}
	defer m.mu.Unlock()
	key := driveKey(kind, project)
	d := m.state.Drives[key]
	if !revision.MatchString(sha) || !m.driveAllowed(d.Drive, owner, generation) {
		return DriveLease{}, ErrDenied
	}
	now := time.Now()
	leases, expired := m.without(func(l Lease) bool { return l.Expires.Before(now) })
	for _, l := range leases {
		if l.Project == project && l.Kind == kind {
			return DriveLease{}, ErrConflict
		}
	}
	name := drivebackup.Name(kind)
	refresh, err := m.drives.Open(refreshPurpose(key), d.Refresh)
	if err != nil {
		d.Blocked, d.Error = true, name+" authorization cannot be opened with this server's seal key; reconnect"
		_ = m.commit(m.leases, nil, []storedDrive{d})
		return DriveLease{}, ErrDenied
	}
	access, err := m.drives.Refresh(ctx, kind, refresh)
	if errors.Is(err, drivebackup.ErrRevoked) {
		d.Blocked, d.Error = true, name+" access was removed or has expired; reconnect to resume backups"
		_ = m.commit(m.leases, nil, []storedDrive{d})
		return DriveLease{}, errors.Join(ErrDenied, err)
	}
	if err != nil {
		d.Error = name + " is unavailable; the worker will retry"
		_ = m.commit(m.leases, nil, []storedDrive{d})
		return DriveLease{}, errors.Join(ErrUnavailable, err)
	}
	// A minute of margin for the worker's clock.
	tokenExpires := access.Expires.Add(-time.Minute).UTC().Truncate(time.Second)
	if tokenExpires.Sub(time.Now()) < minimumTokenLife {
		d.Error = name + " issued an access token that expires too soon; the worker will retry"
		_ = m.commit(m.leases, nil, []storedDrive{d})
		return DriveLease{}, ErrUnavailable
	}
	// Authority can change while the provider answers.
	if !m.driveAllowed(d.Drive, owner, generation) {
		return DriveLease{}, ErrDenied
	}
	if access.Refresh != "" {
		if d.Refresh, err = m.drives.Seal(refreshPurpose(key), access.Refresh); err != nil {
			return DriveLease{}, err
		}
	}
	var encryption *Encryption
	if d.Encrypted {
		password, err := m.drives.Open(passwordPurpose(key), d.Password)
		if err != nil {
			return DriveLease{}, errors.Join(ErrUnavailable, errors.New("the destination's encryption password cannot be opened with this server's seal key"))
		}
		encryption = &Encryption{Password: password}
	}
	var id [32]byte
	rand.Read(id[:])
	l := Lease{NotebookGeneration: d.NotebookGeneration, ID: hex.EncodeToString(id[:]), Project: project, Kind: kind, Owner: owner, Generation: generation, Revision: sha, Expires: time.Now().Add(3 * time.Minute).UTC().Truncate(time.Second)}
	d.LastAttempt = time.Now().UTC()
	leases[l.ID] = l
	if err = m.commit(leases, nil, []storedDrive{d}); err != nil {
		return DriveLease{}, err
	}
	m.revoke(ctx, expired...)
	return DriveLease{ID: l.ID, Project: project, Kind: kind, Generation: generation, NotebookGeneration: d.NotebookGeneration, Revision: sha, Path: d.Path, Token: access.Token, TokenExpires: tokenExpires, Expires: l.Expires, Encryption: encryption}, nil
}

// checkedDrive is a worker's active drive lease and its destination; a lease
// that lapsed or lost its authority ends.
func (m *Manager) checkedDrive(owner, id, kind string) (Lease, storedDrive, error) {
	if err := m.Outgoing.Check(); err != nil {
		return Lease{}, storedDrive{}, err
	}
	l, ok := m.leases[id]
	if !ok || l.Owner != owner || l.Kind == "" || l.Kind != kind {
		return Lease{}, storedDrive{}, ErrDenied
	}
	d := m.state.Drives[driveKey(kind, l.Project)]
	if l.Expires.Before(time.Now()) || !m.driveAllowed(d.Drive, owner, l.Generation) {
		leases, _ := m.without(func(other Lease) bool { return other.ID == id })
		_ = m.commit(leases, nil, nil)
		return Lease{}, d, ErrDenied
	}
	return l, d, nil
}

// CompleteDrive records a worker's receipt for its drive lease and ends the
// lease. A receipt never stores the worker's error text.
func (m *Manager) CompleteDrive(ctx context.Context, owner string, in DriveReceipt) (Drive, error) {
	if err := m.lock(); err != nil {
		return Drive{}, err
	}
	defer m.mu.Unlock()
	l, d, err := m.checkedDrive(owner, in.Lease, in.Kind)
	if err != nil {
		return Drive{}, err
	}
	if in.Project != l.Project || in.Generation != l.Generation || in.Revision != l.Revision {
		return Drive{}, ErrDenied
	}
	name := drivebackup.Name(in.Kind)
	switch {
	case in.Error == "" && !in.Diverged:
		s := in.Snapshot
		if s == nil || !snapshotName.MatchString(s.Name) || !sha256Hex.MatchString(s.SHA256) || s.Size <= 0 || s.Kept < 1 || len(s.Deleted) > 1000 {
			return d.Drive, errors.Join(ErrDenied, errors.New("a successful drive receipt names its verified snapshot"))
		}
		for _, deleted := range s.Deleted {
			if !snapshotName.MatchString(deleted) {
				return d.Drive, errors.Join(ErrDenied, errors.New("invalid deleted snapshot name"))
			}
		}
		d.LastRevision, d.LastSnapshot, d.LastSHA256, d.LastSize, d.Kept = l.Revision, s.Name, s.SHA256, s.Size, s.Kept
		d.LastSuccess = time.Now().UTC()
		d.Error = ""
	case in.Diverged:
		d.Blocked = true
		d.Error = "The " + name + " folder changed outside ForgetMeNote. Check it, then explicitly resume backups."
	default:
		d.Error = "Upload to " + name + " failed; the worker will retry"
	}
	leases, _ := m.without(func(other Lease) bool { return other.ID == l.ID })
	if err = m.commit(leases, nil, []storedDrive{d}); err != nil {
		return d.Drive, err
	}
	return d.Drive, nil
}

// AcknowledgeDriveDrain records that the worker stopped uploading to a drive
// destination for a handoff.
func (m *Manager) AcknowledgeDriveDrain(owner, kind, project, id string, generation uint64) error {
	if err := m.lock(); err != nil {
		return err
	}
	defer m.mu.Unlock()
	d, ok := m.state.Drives[driveKey(kind, project)]
	if !ok || d.Owner != owner || d.Handoff != id || id == "" || d.Enabled || d.Generation != generation {
		return ErrDenied
	}
	d.Drained = true
	return m.commit(m.leases, nil, []storedDrive{d})
}

func (m *Manager) listDrives(owner string) []DriveEntry {
	list := []Drive{}
	for _, d := range m.state.Drives {
		if d.Owner == owner && (d.Handoff != "" && !d.Drained || m.driveAllowed(d.Drive, owner, d.Generation)) {
			list = append(list, d.Drive)
		}
	}
	sortDrives(list)
	out := []DriveEntry{}
	for _, d := range list {
		out = append(out, DriveEntry{Kind: d.Kind, Project: d.Project, Generation: d.Generation, NotebookGeneration: d.NotebookGeneration, Path: d.Path, Encrypted: d.Encrypted, Enabled: d.Enabled, Blocked: d.Blocked, Request: d.Request, Handoff: d.Handoff, LastRevision: d.LastRevision, LastSnapshot: d.LastSnapshot})
	}
	return out
}

// ListDrives is the drive part of Worklist.
func (m *Manager) ListDrives(owner string) ([]DriveEntry, error) {
	if err := m.lock(); err != nil {
		return nil, err
	}
	defer m.mu.Unlock()
	return m.listDrives(owner), nil
}

// fenceDrives stops a notebook's connected drive destinations for handoff id.
func (m *Manager) fenceDrives(project, id string) []storedDrive {
	out := []storedDrive{}
	for _, d := range m.state.Drives {
		if d.Project == project && d.Connected {
			d.Handoff, d.Enabled, d.Drained = id, false, false
			d.Generation++
			out = append(out, d)
		}
	}
	return out
}

// drivesDrained reports whether the worker acknowledged handoff id for every
// drive destination it fenced.
func (m *Manager) drivesDrained(project, id string) bool {
	for _, d := range m.state.Drives {
		if d.Project == project && d.Handoff == id && !d.Drained {
			return false
		}
	}
	return true
}

// driveGrant is a refresh token to revoke at its provider.
type driveGrant struct{ kind, refresh string }

// finishDrives ends handoff id for its drive destinations. A committed handoff
// disconnects them, as the notebook no longer holds editable notes; it
// returns their grants to revoke. An aborted one leaves them paused.
func (m *Manager) finishDrives(project, id string, commit bool) ([]storedDrive, []driveGrant) {
	out, grants := []storedDrive{}, []driveGrant{}
	for key, d := range m.state.Drives {
		if d.Project != project || d.Handoff != id || id == "" {
			continue
		}
		d.Handoff, d.Drained, d.Enabled = "", false, false
		d.Generation++
		if commit {
			if !m.sharedGrant(d.Drive, key) && m.drives.Configured(d.Kind) {
				if refresh, err := m.drives.Open(refreshPurpose(key), d.Refresh); err == nil {
					grants = append(grants, driveGrant{d.Kind, refresh})
				}
			}
			d.Connected, d.Refresh, d.Password = false, "", ""
		}
		out = append(out, d)
	}
	return out, grants
}
