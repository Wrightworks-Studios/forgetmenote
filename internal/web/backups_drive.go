package web

import (
	"cmp"
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"slices"
	"time"

	"forgetmenote/internal/backups"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/drivebackup"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

// driveBinding ties a pending drive connection to the browser that started it,
// apart from a GitHub backup connection the same browser may have pending.
var driveBinding = connectionBinding{name: "fmn-drive-connect", path: "/"}

// A drivePending is a drive connection between its start and the provider's
// callback; a driveGrant is the grant the callback obtained, waiting for the
// Owner's consent on the setup screen. The grant's refresh token is sealed
// under the drive seal key, which only gateways hold.
type drivePending struct {
	Kind       string    `json:"kind"`
	Project    string    `json:"project"`
	Subject    string    `json:"subject"`
	Binding    string    `json:"binding"`
	Verifier   string    `json:"verifier,omitempty"`
	Generation uint64    `json:"generation"`
	Expires    time.Time `json:"expires"`
	Refresh    string    `json:"sealedRefresh,omitempty"`
	Account    string    `json:"account,omitempty"`
	AccountID  string    `json:"accountId,omitempty"`
	Path       string    `json:"path,omitempty"`
}

func (p drivePending) purpose() string {
	return "pending " + p.Kind + ":" + p.Project + ":" + p.Binding
}

// gitHubBackups is the managed backup service when it runs the Backups GitHub
// App.
func (s *Server) gitHubBackups() (*backupService, error) {
	service, err := s.managedBackups()
	if err == nil && service.app == nil {
		err = backups.ErrUnavailable
	}
	return service, err
}

// driveBackups is the managed backup service when it runs the drive kind.
func (s *Server) driveBackups(kind string) (*backupService, error) {
	service, err := s.managedBackups()
	if err == nil && !service.drives.Configured(kind) {
		err = backups.ErrUnavailable
	}
	return service, err
}

// personalNotebook is the account a protected Inbox or assistant notebook
// belongs to, or "" for a notebook that is not personal. Workers' polls check
// every destination, so an ordinary notebook answers without a listing.
func (s *Server) personalNotebook(project string) string {
	if s.catalog.OrdinaryNotebook(project) {
		return ""
	}
	for _, p := range s.catalog.List(catalog.Bootstrap) {
		if p.ID == project {
			return cmp.Or(p.ProtectedInbox, p.AssistantOwner)
		}
	}
	return ""
}

// personalBackupAllowed admits a backup destination of a personal notebook
// only from its own account. An Admin's authority over another person's Inbox
// or assistant notebook never sends its history to a destination.
func (s *Server) personalBackupAllowed(user, project string) error {
	owner := s.personalNotebook(project)
	if owner == "" {
		return nil
	}
	if a, err := s.catalog.Account(user); err != nil || a.ID != owner {
		return workspace.ErrForbidden
	}
	return nil
}

// A driveStatus is one configured drive kind of a notebook, connected or not.
type driveStatus struct {
	backups.Drive
	Name    string `json:"name"`
	Pending bool   `json:"pending"`
}

func (s *Server) driveView(d backups.Drive, head string) driveStatus {
	d.AccountID = ""
	return driveStatus{Drive: d, Name: drivebackup.Name(d.Kind), Pending: d.Connected && head != "" && d.LastRevision != head}
}

func (s *Server) driveStatus(project, head string) ([]driveStatus, error) {
	out := []driveStatus{}
	if s.backups == nil {
		return out, nil
	}
	drives, err := s.backups.manager.DriveStatus(project)
	if err != nil {
		return nil, err
	}
	for _, kind := range s.backups.drives.Kinds() {
		d := backups.Drive{Kind: kind, Project: project}
		for _, stored := range drives {
			if stored.Kind == kind {
				d = stored
			}
		}
		out = append(out, s.driveView(d, head))
	}
	return out, nil
}

func driveCallbackPath(kind string) string {
	return map[string]string{drivebackup.Google: "/backup/google/callback", drivebackup.Dropbox: "/backup/dropbox/callback"}[kind]
}

// driveRoutes connect a drive destination: the Owner starts the provider's
// authorization, the callback keeps the grant, and the setup screen shows the
// account and folder and takes the Owner's consent and encryption choice.
func (s *Server) driveRoutes(register routeRegistrar) {
	register.project("POST /api/managed-backup/drive/connect", "owner", s.driveConnect)
	register.project("GET /api/managed-backup/drive/pending", "owner", s.drivePendingStatus)
	register.project("POST /api/managed-backup/drive/enable", "owner", s.driveEnable)
	register.project("POST /api/managed-backup/drive/cancel", "owner", s.driveCancel)
	for _, kind := range drivebackup.Kinds {
		register.global("GET "+driveCallbackPath(kind), func(w http.ResponseWriter, r *http.Request) { s.driveCallback(w, r, kind) })
	}
}

// driveReady is the service for a browser request that may act on the
// selected notebook's destination of kind; otherwise it writes the refusal.
func (s *Server) driveReady(w http.ResponseWriter, r *http.Request, kind string) (*backupService, bool) {
	project := projectID(r.Context())
	if s.catalog.Kind(project) != "notes" {
		fail(w, workspace.ErrForbidden)
		return nil, false
	}
	service, err := s.driveBackups(kind)
	if err == nil {
		err = s.personalBackupAllowed(subject(r.Context()), project)
	}
	if err == nil {
		err = s.outgoing.Check()
	}
	if err != nil {
		fail(w, err)
		return nil, false
	}
	return service, true
}

func (s *Server) driveConnect(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind string `json:"kind"`
	}
	if !decode(w, r, &in) {
		return
	}
	service, ok := s.driveReady(w, r, in.Kind)
	if !ok {
		return
	}
	a, err := s.catalog.Account(subject(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	state, binding, verifier := RandomToken(), RandomToken(), RandomToken()
	p := drivePending{Kind: in.Kind, Project: projectID(r.Context()), Subject: subject(r.Context()), Binding: digest(binding), Verifier: verifier, Generation: a.Generation, Expires: time.Now().Add(10 * time.Minute)}
	address, err := service.drives.AuthorizeURL(in.Kind, s.origin+driveCallbackPath(in.Kind), state, verifier)
	if err != nil {
		fail(w, err)
		return
	}
	if err = s.beginFlow(r.Context(), "drive/pending", digest(state), p.Subject, p, 10*time.Minute, pg.FlowBound{Limit: flowLimit, Replace: true}); err != nil {
		fail(w, err)
		return
	}
	s.setBinding(w, driveBinding, binding)
	jsonResponse(w, map[string]string{"url": address})
}

// driveAuthority rechecks the person a pending connection belongs to: the
// same sign-in, still the notebook's Owner, and its own person when it is
// personal.
func (s *Server) driveAuthority(p drivePending) error {
	a, err := s.catalog.Account(p.Subject)
	if err != nil || a.Generation != p.Generation || p.Expires.Before(time.Now()) || s.catalog.Kind(p.Project) != "notes" {
		return backups.ErrDenied
	}
	if err = s.personalBackupAllowed(p.Subject, p.Project); err != nil {
		return err
	}
	_, err = s.catalog.Access(p.Subject, p.Project, "owner")
	return err
}

func (s *Server) driveCallback(w http.ResponseWriter, r *http.Request, kind string) {
	service, err := s.driveBackups(kind)
	if err == nil {
		err = s.outgoing.Check()
	}
	if err != nil {
		fail(w, err)
		return
	}
	binding, err := s.binding(r, driveBinding)
	if err != nil {
		fail(w, backups.ErrDenied)
		return
	}
	p, valid, err := claimFlow(s, r.Context(), "drive/pending", digest(r.URL.Query().Get("state")), func(p drivePending) bool {
		return p.Kind == kind && p.Subject == subject(r.Context()) && p.Expires.After(time.Now()) && subtle.ConstantTimeCompare([]byte(p.Binding), []byte(digest(binding))) == 1
	})
	if err != nil {
		fail(w, err)
		return
	}
	code := r.URL.Query().Get("code")
	if !valid || s.driveAuthority(p) != nil || code == "" || len(code) > 2048 || r.URL.Query().Get("error") != "" {
		fail(w, backups.ErrDenied)
		return
	}
	grant, err := service.drives.Exchange(r.Context(), kind, code, p.Verifier, s.origin+driveCallbackPath(kind))
	if err != nil {
		fail(w, err)
		return
	}
	sealed, err := service.drives.Seal(p.purpose(), grant.Refresh)
	if err == nil {
		err = s.driveAuthority(p)
	}
	if err != nil {
		_ = service.manager.AbandonGrant(r.Context(), kind, grant.AccountID, grant.Refresh)
		fail(w, backups.ErrDenied)
		return
	}
	p.Verifier, p.Refresh, p.Account, p.AccountID, p.Path = "", sealed, grant.Account, grant.AccountID, backups.DrivePath(p.Project)
	if err = s.putFlow(r.Context(), "drive/grant", p.Binding, p.Subject, p, p.Expires); err != nil {
		_ = service.manager.AbandonGrant(r.Context(), kind, grant.AccountID, grant.Refresh)
		fail(w, err)
		return
	}
	http.Redirect(w, r, "/?project="+url.QueryEscape(p.Project)+"&backup="+url.QueryEscape(kind), http.StatusSeeOther)
}

// driveGrant is the browser's pending grant for kind in the selected notebook,
// with its refresh token opened.
func (s *Server) driveGrant(r *http.Request, service *backupService, kind string) (drivePending, string, error) {
	binding, err := s.binding(r, driveBinding)
	if err != nil {
		return drivePending{}, "", backups.ErrDenied
	}
	p, ok, err := readFlow[drivePending](s, r.Context(), "drive/grant", digest(binding))
	if err != nil {
		return p, "", err
	}
	if !ok || p.Kind != kind || p.Subject != subject(r.Context()) || p.Project != projectID(r.Context()) || p.Refresh == "" {
		return p, "", backups.ErrDenied
	}
	if err = s.driveAuthority(p); err != nil {
		return p, "", err
	}
	refresh, err := service.drives.Open(p.purpose(), p.Refresh)
	if err != nil {
		return p, "", backups.ErrDenied
	}
	return p, refresh, nil
}

// drivePendingStatus is what the setup screen shows before the Owner's
// consent: the account the grant belongs to and the folder snapshots will go
// to. Reconnecting the same account keeps its folder and encryption.
func (s *Server) drivePendingStatus(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	service, ok := s.driveReady(w, r, kind)
	if !ok {
		return
	}
	p, _, err := s.driveGrant(r, service, kind)
	if err != nil {
		fail(w, err)
		return
	}
	out := map[string]any{"kind": kind, "name": drivebackup.Name(kind), "account": p.Account, "path": p.Path, "continuing": false, "encrypted": false}
	drives, err := service.manager.DriveStatus(p.Project)
	if err != nil {
		fail(w, err)
		return
	}
	for _, d := range drives {
		if d.Kind == kind {
			out["generation"] = d.Generation
			if d.Connected && d.AccountID == p.AccountID {
				out["path"], out["continuing"], out["encrypted"] = d.Path, true, d.Encrypted
			}
		}
	}
	jsonResponse(w, out)
}

func (s *Server) driveEnable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind       string `json:"kind"`
		Generation uint64 `json:"generation"`
		Encrypt    bool   `json:"encrypt"`
		Consent    bool   `json:"consent"`
	}
	if !decode(w, r, &in) {
		return
	}
	service, ok := s.driveReady(w, r, in.Kind)
	if !ok {
		return
	}
	if !in.Consent {
		http.Error(w, "Confirm that anyone with access to the folder can read every snapshot unless it is encrypted", 400)
		return
	}
	p, refresh, err := s.driveGrant(r, service, in.Kind)
	if err != nil {
		fail(w, err)
		return
	}
	setup := backups.DriveSetup{Drive: backups.Drive{Kind: p.Kind, Project: p.Project, Actor: p.Subject, Owner: s.catalog.StorageOwner(p.Project), Epoch: s.catalog.Epoch(p.Subject), Authority: s.catalog.ProjectAccessEpoch(p.Subject, p.Project), NotebookGeneration: s.catalog.Authority(p.Project).Generation, Account: p.Account, AccountID: p.AccountID, Path: p.Path, Generation: in.Generation}, Refresh: refresh, Encrypt: in.Encrypt}
	type connected struct {
		Drive    driveStatus `json:"drive"`
		Password string      `json:"password,omitempty"`
	}
	out, err := integrationActivity(s, p.Subject, "backup."+p.Kind+".connect", p.Project, func() (connected, error) {
		d, password, err := service.manager.ConnectDrive(r.Context(), setup)
		return connected{s.driveView(d, ""), password}, err
	})
	if err != nil {
		fail(w, err)
		return
	}
	// The grant is the destination's now; the setup is done.
	_ = s.endFlow(r.Context(), "drive/grant", p.Binding)
	s.clearBinding(w, driveBinding)
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, out)
}

// driveCancel ends a setup the Owner declined and revokes its unused grant.
func (s *Server) driveCancel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind string `json:"kind"`
	}
	if !decode(w, r, &in) {
		return
	}
	service, ok := s.driveReady(w, r, in.Kind)
	if !ok {
		return
	}
	p, refresh, err := s.driveGrant(r, service, in.Kind)
	if err != nil {
		fail(w, err)
		return
	}
	if err = s.endFlow(r.Context(), "drive/grant", p.Binding); err != nil {
		fail(w, err)
		return
	}
	s.clearBinding(w, driveBinding)
	respond(w, map[string]bool{"ok": true}, service.manager.AbandonGrant(r.Context(), p.Kind, p.AccountID, refresh))
}

// A backupOverview is every notebook's managed destinations, for an Admin.
type backupOverview struct {
	Configured bool                `json:"configured"`
	GitHub     bool                `json:"github"`
	Drives     []string            `json:"drives"`
	Notebooks  []backupOverviewRow `json:"notebooks"`
}
type backupOverviewRow struct {
	Project      string              `json:"project"`
	Name         string              `json:"name"`
	Owners       []string            `json:"owners"`
	Personal     bool                `json:"personal,omitempty"`
	Destinations []backupDestination `json:"destinations"`
}
type backupDestination struct {
	Kind         string    `json:"kind"`
	Name         string    `json:"name"`
	Target       string    `json:"target"`
	Account      string    `json:"account,omitempty"`
	Encrypted    bool      `json:"encrypted,omitempty"`
	Enabled      bool      `json:"enabled"`
	Blocked      bool      `json:"blocked,omitempty"`
	LastRevision string    `json:"lastRevision,omitempty"`
	LastSuccess  time.Time `json:"lastSuccess,omitzero"`
	Error        string    `json:"error,omitempty"`
}

// backupOverview lists each notebook that can be backed up with its connected
// destinations, notebooks without one first. Personal notebooks show no
// provider account.
func (s *Server) backupOverview(actor string) (backupOverview, error) {
	if !s.catalog.IsAdmin(actor) {
		return backupOverview{}, workspace.ErrForbidden
	}
	out := backupOverview{Drives: []string{}, Notebooks: []backupOverviewRow{}}
	users, projects, err := s.catalog.Administration(actor)
	if err != nil {
		return out, err
	}
	bindings, drives := map[string]backups.Binding{}, map[string][]backups.Drive{}
	if s.backups != nil {
		out.Configured, out.GitHub, out.Drives = true, s.backups.app != nil, s.backups.drives.Kinds()
		if bindings, drives, err = s.backups.manager.Overview(); err != nil {
			return out, err
		}
	}
	names := map[string]string{}
	for _, u := range users {
		names[u.ID] = cmp.Or(u.DisplayName, u.ID)
	}
	for _, p := range projects {
		if cmp.Or(p.Kind, "notes") != "notes" {
			continue
		}
		row := backupOverviewRow{Project: p.ID, Name: p.Name, Owners: []string{}, Personal: p.ProtectedInbox != "" || p.AssistantOwner != "", Destinations: []backupDestination{}}
		for id, role := range p.Members {
			if role == "owner" && id != catalog.Bootstrap {
				row.Owners = append(row.Owners, cmp.Or(names[id], id))
			}
		}
		slices.Sort(row.Owners)
		if b := bindings[p.ID]; b.Connected {
			row.Destinations = append(row.Destinations, backupDestination{Kind: "github", Name: "GitHub", Target: b.Repository.FullName + " " + b.Branch, Enabled: b.Enabled, Blocked: b.Blocked, LastRevision: b.LastRevision, LastSuccess: b.LastSuccess, Error: b.Error})
		}
		for _, d := range drives[p.ID] {
			if !d.Connected {
				continue
			}
			destination := backupDestination{Kind: d.Kind, Name: drivebackup.Name(d.Kind), Target: d.Path, Account: d.Account, Encrypted: d.Encrypted, Enabled: d.Enabled, Blocked: d.Blocked, LastRevision: d.LastRevision, LastSuccess: d.LastSuccess, Error: d.Error}
			if row.Personal {
				destination.Account = ""
			}
			row.Destinations = append(row.Destinations, destination)
		}
		out.Notebooks = append(out.Notebooks, row)
	}
	slices.SortStableFunc(out.Notebooks, func(a, b backupOverviewRow) int {
		if (len(a.Destinations) == 0) != (len(b.Destinations) == 0) {
			if len(a.Destinations) == 0 {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Project, b.Project))
	})
	return out, nil
}

// driveConnections lists the drive destinations a person connected, for
// offboarding, whether or not the module runs now.
func (s *Server) driveConnections(ctx context.Context, user string, disabled bool) ([]map[string]any, error) {
	var drives []backups.Drive
	var err error
	if s.backups != nil {
		var all map[string][]backups.Drive
		if _, all, err = s.backups.manager.Overview(); err != nil {
			return nil, err
		}
		for _, list := range all {
			drives = append(drives, list...)
		}
	} else if !s.modules.Enabled("managed-backups") {
		if drives, err = backups.StoredDrives(ctx, s.db); err != nil {
			return nil, err
		}
	}
	out := []map[string]any{}
	for _, d := range drives {
		if d.Actor == user && d.Connected {
			out = append(out, map[string]any{"project": d.Project, "kind": "backup", "destination": d.Kind, "paused": !d.Enabled, "reauthorize": disabled || s.catalog.Epoch(user) != d.Epoch})
		}
	}
	slices.SortFunc(out, func(a, b map[string]any) int {
		return cmp.Or(cmp.Compare(a["project"].(string), b["project"].(string)), cmp.Compare(a["destination"].(string), b["destination"].(string)))
	})
	return out, nil
}
