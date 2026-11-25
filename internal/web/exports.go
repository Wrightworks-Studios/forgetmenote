package web

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// An export snapshot is a row and its bundle an object in blob storage, so any
// gateway serves an export's next chunk. Each belongs to the authenticated user
// and notebook that began it and lasts fifteen minutes; every tool call still
// passes current authorization first. A bundle is stored only after its row
// commits, and removed after its row, so the sweep, which lists bundles before
// it reads rows, never removes the bundle of a live export.
type exportCache struct {
	mu     sync.Mutex
	timer  *time.Timer
	closed bool
	// sweep removes the instance's expired pending flows (sweepFlows).
	sweep *time.Timer
}

const (
	maxExportBytes int64 = 1 << 30
	// exportPiece is the largest chunk a call returns.
	exportPiece = 262144
	exportLife  = 15 * time.Minute
	liveExports = 2
)

var (
	// errExportGone is a later chunk of an export that expired or ended, as
	// when a purge ended it: the client starts the export again at offset
	// zero, which this call, with its offset and checksum, cannot do.
	errExportGone = webcore.Error{Status: http.StatusGone, Code: "export_expired", Message: "export expired or unavailable; restart at offset zero"}
	// errExportPurged is an export a purge ended while its first chunk was
	// prepared; the same call, at offset zero, makes it again.
	errExportPurged = webcore.Error{Status: http.StatusServiceUnavailable, Code: "timeout", Message: "the notebook was purged while its export was prepared; start the export again at offset zero"}
	// errExportFenced is an export that waited too long for a purge to end;
	// the same call succeeds once it has.
	errExportFenced = webcore.Error{Status: http.StatusServiceUnavailable, Code: "timeout", Message: "a purge is running; start the export again shortly"}
)

// exportLockWait is how long a new export waits for the lock that counts
// exports, which a purge holds while it rewrites a notebook (fenceExports),
// keeping a database connection meanwhile.
var exportLockWait = 5 * time.Second

func exportID(user, project, sha string) string {
	return digest(user + "\x00" + project + "\x00" + sha)
}
func exportKey(id string) string { return "exports/" + id }
func exportContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

func (s *Server) closeExports() {
	s.exports.mu.Lock()
	defer s.exports.mu.Unlock()
	s.exports.closed = true
	if s.exports.timer != nil {
		s.exports.timer.Stop()
	}
	if s.exports.sweep != nil {
		s.exports.sweep.Stop()
	}
}

// expireExports removes the instance's expired exports and any bundle whose
// row is gone. Any gateway may; the one that began an export does every minute
// while it is live.
func (s *Server) expireExports() {
	ctx, cancel := exportContext()
	defer cancel()
	bundles, err := s.blobs.List(ctx, "exports/")
	s.db.Pool.Exec(ctx, `DELETE FROM exports WHERE org = $1 AND expires <= now()`, s.db.Org)
	ids := map[string]bool{}
	if err == nil {
		var rows pgx.Rows
		if rows, err = s.db.Pool.Query(ctx, `SELECT id FROM exports WHERE org = $1`, s.db.Org); err == nil {
			var id string
			_, err = pgx.ForEachRow(rows, []any{&id}, func() error { ids[id] = true; return nil })
		}
	}
	if err == nil {
		for _, key := range bundles {
			// Bundles lie directly under exports/. Kept in the data
			// directory, it also holds the notebooks' Git exports
			// (exports/ID.git), which are not this sweep's.
			if id := strings.TrimPrefix(key, "exports/"); !strings.Contains(id, "/") && !ids[id] {
				s.blobs.Delete(ctx, key)
			}
		}
	}
	live := len(ids)
	s.exports.mu.Lock()
	defer s.exports.mu.Unlock()
	if !s.exports.closed && s.exports.sweep == nil {
		// The first pass, as the gateway starts, begins the flows' sweep.
		s.exports.sweep = time.AfterFunc(0, s.sweepFlows)
	}
	s.exports.timer = nil
	if !s.exports.closed && (err != nil || live > 0) {
		s.exports.timer = time.AfterFunc(time.Minute, s.expireExports)
	}
}

// flowSweep is how often a gateway removes the instance's expired pending
// flows, and keeps the purge receipts the database refused.
const flowSweep = 15 * time.Minute

// sweepFlows removes the instance's expired pending flows, of every kind,
// then again every flowSweep while the gateway runs. A flow nobody finishes
// would otherwise keep its body, such as the text an agent's purge plan
// would erase, until another flow of its group began.
func (s *Server) sweepFlows() {
	ctx, cancel := exportContext()
	defer cancel()
	s.db.SweepFlows(ctx)
	// And as often, a purge receipt the database refused is kept.
	s.keepSpooledPurgeReceipts(ctx)
	s.exports.mu.Lock()
	defer s.exports.mu.Unlock()
	if s.exports.sweep != nil {
		s.exports.sweep.Stop()
	}
	if !s.exports.closed {
		s.exports.sweep = time.AfterFunc(flowSweep, s.sweepFlows)
	}
}

func (s *Server) watchExports() {
	s.exports.mu.Lock()
	defer s.exports.mu.Unlock()
	if !s.exports.closed && s.exports.timer == nil {
		s.exports.timer = time.AfterFunc(time.Minute, s.expireExports)
	}
}

func (s *Server) backupChunk(ctx context.Context, store *workspace.Store, project string, offset int64, limit int, expected string) (binaryChunk, error) {
	if offset < 0 || limit < 0 || limit > exportPiece || offset > 0 && expected == "" {
		return binaryChunk{}, errors.New("provide valid chunk bounds and sha256 after the first chunk")
	}
	s.exports.mu.Lock()
	closed := s.exports.closed
	s.exports.mu.Unlock()
	if closed {
		return binaryChunk{}, errors.New("server shutting down")
	}
	user := subject(ctx)
	sha, total := expected, int64(0)
	var err error
	if expected == "" {
		sha, total, err = s.beginExport(ctx, store, user, project)
	} else {
		total, err = s.exportTotal(ctx, exportID(user, project, expected))
	}
	if err != nil {
		return binaryChunk{}, err
	}
	id := exportID(user, project, sha)
	out, err := chunk(exportReader{ctx: ctx, s: s, id: id}, total, offset, limit)
	out.SHA256 = sha
	out.MIMEType = "application/octet-stream"
	if err == nil && out.NextOffset == out.Total {
		end, cancel := exportContext()
		defer cancel()
		s.dropExport(end, id)
	}
	return out, err
}

// dropExport ends export id: its row, then its bundle.
func (s *Server) dropExport(ctx context.Context, id string) {
	if _, err := s.db.Pool.Exec(ctx, `DELETE FROM exports WHERE org = $1 AND id = $2`, s.db.Org, id); err == nil {
		s.blobs.Delete(ctx, exportKey(id))
	}
}

// bundleHeads returns the commits the references of a Git bundle name, read
// from its header (bundle format v2 or v3).
func bundleHeads(bundle io.Reader) ([]string, error) {
	header := bufio.NewReader(bundle)
	signature, err := header.ReadString('\n')
	if err != nil || signature != "# v2 git bundle\n" && signature != "# v3 git bundle\n" {
		return nil, errors.New("the export is not a Git bundle")
	}
	var heads []string
	for {
		line, err := header.ReadString('\n')
		if err != nil {
			return nil, errors.New("the export's bundle header is incomplete")
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			return heads, nil
		case strings.HasPrefix(line, "@") || strings.HasPrefix(line, "-"):
			continue // a capability, or a prerequisite commit
		}
		sha, _, ok := strings.Cut(line, " ")
		if !ok || len(sha) != 40 {
			return nil, errors.New("the export's bundle header is invalid")
		}
		heads = append(heads, sha)
	}
}

func (s *Server) exportTotal(ctx context.Context, id string) (int64, error) {
	var total int64
	err := s.db.Pool.QueryRow(ctx, `SELECT total FROM exports WHERE org = $1 AND id = $2 AND expires > now()`, s.db.Org, id).Scan(&total)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errExportGone
	}
	if err != nil {
		return 0, errors.New("exports are unavailable; retry shortly")
	}
	return total, nil
}

// beginExport writes store's bundle to a temporary file, records the export
// and then stores the bundle. An export of the same user, notebook and bundle
// replaces the old one.
//
// A purge ends the exports in progress before it rewrites a notebook, and no
// export records itself until the rewrite is done (fenceExports), since
// their bundles hold the notebook as it was; but one whose bundle was made
// before the purge may record and store it after that. So once an export's
// row and bundle are stored it checks that every commit the bundle's
// references name is still one of the notebook's revisions, and otherwise
// ends itself (purgedBundle). A purge that commits after the check found the
// stored export and ended it before its rewrite; a check before the row
// commits or the bundle is stored would miss a purge that runs in between.
func (s *Server) beginExport(ctx context.Context, store *workspace.Store, user, project string) (string, int64, error) {
	full := fmt.Errorf("%d exports are already active; complete them or retry after expiry", liveExports)
	var live int
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM exports WHERE org = $1 AND expires > now()`, s.db.Org).Scan(&live); err != nil {
		return "", 0, errors.New("exports are unavailable; retry shortly")
	}
	if live >= liveExports {
		return "", 0, full
	}
	dir := filepath.Join(s.data, "cache", "exports")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", 0, err
	}
	file, err := os.CreateTemp(dir, "bundle-*")
	if err != nil {
		return "", 0, err
	}
	name := file.Name()
	file.Close()
	defer os.Remove(name)
	if err = store.BackupLimited(ctx, name, maxExportBytes); err != nil {
		return "", 0, err
	}
	// A purge removes the bundles being made (removeTemporaryBundles), of
	// whichever notebook.
	if file, err = os.Open(name); errors.Is(err, fs.ErrNotExist) {
		return "", 0, errExportPurged
	} else if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	total, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	sha := hex.EncodeToString(hash.Sum(nil))
	heads, err := bundleHeads(io.NewSectionReader(file, 0, total))
	if err != nil {
		return "", 0, err
	}
	id := exportID(user, project, sha)
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return "", 0, errors.New("exports are unavailable; retry shortly")
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	// The bound holds across gateways: each new export counts the others under
	// one lock, the lock a purge holds (fenceExports).
	if _, err = tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, fmt.Sprint(exportLockWait.Milliseconds())); err != nil {
		return "", 0, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('exports ' || $1, 0))`, s.db.Org); err != nil {
		if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return "", 0, errExportFenced
		}
		return "", 0, err
	}
	rows, err := tx.Query(ctx, `DELETE FROM exports WHERE org = $1 AND (expires <= now() OR id = $2) RETURNING id`, s.db.Org, id)
	if err != nil {
		return "", 0, err
	}
	expired, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", 0, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM exports WHERE org = $1`, s.db.Org).Scan(&live); err != nil {
		return "", 0, err
	}
	if live >= liveExports {
		return "", 0, full
	}
	if _, err = tx.Exec(ctx, `INSERT INTO exports (org, id, sha, total, expires) VALUES ($1, $2, $3, $4, now() + $5 * interval '1 millisecond')`, s.db.Org, id, sha, total, exportLife.Milliseconds()); err != nil {
		return "", 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", 0, errors.New("exports are unavailable; retry shortly")
	}
	s.watchExports()
	for _, old := range expired {
		if old != id {
			s.blobs.Delete(ctx, exportKey(old))
		}
	}
	if err = s.blobs.Put(ctx, exportKey(id), file); err != nil {
		end, cancel := exportContext()
		defer cancel()
		s.db.Pool.Exec(end, `DELETE FROM exports WHERE org = $1 AND id = $2`, s.db.Org, id)
		return "", 0, errors.New("exports are unavailable; retry shortly")
	}
	end, cancel := exportContext()
	defer cancel()
	if purged, err := purgedBundle(end, store, heads); err != nil || purged {
		s.dropExport(end, id)
		if err != nil {
			return "", 0, errors.New("exports are unavailable; retry shortly")
		}
		return "", 0, errExportPurged
	}
	// A purge that ended the export (fenceExports) once its row committed
	// may have removed the bundle before it was stored; the first chunk,
	// like every other, is read only while the row is there.
	if _, err := s.exportTotal(end, id); err != nil {
		if errors.Is(err, errExportGone) {
			s.blobs.Delete(end, exportKey(id))
			// The same call starts it again.
			err = errExportPurged
		}
		return "", 0, err
	}
	return sha, total, nil
}

// purgedBundle reports whether a purge rewrote or removed a commit that a
// bundle's references, heads, name: then the bundle holds the notebook as it
// was. A purge rewrites every revision from the first one it changes, so it
// rewrote those commits exactly when it changed anything the bundle holds.
func purgedBundle(ctx context.Context, store *workspace.Store, heads []string) (bool, error) {
	for _, head := range heads {
		now, _, err := store.ResolvePurgedRevision(ctx, "", head)
		if errors.Is(err, fs.ErrNotExist) || err == nil && now != head {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
	return false, nil
}

// exportReader reads a stored export's bytes at any offset.
type exportReader struct {
	ctx context.Context
	s   *Server
	id  string
}

func (r exportReader) ReadAt(p []byte, offset int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	bundle, err := r.s.blobs.GetRange(r.ctx, exportKey(r.id), offset, int64(len(p)))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, errExportGone
	}
	if err != nil {
		return 0, errors.New("exports are unavailable; retry shortly")
	}
	defer bundle.Close()
	read, err := io.ReadFull(bundle, p)
	if err != nil {
		return read, errors.New("exports are unavailable; retry shortly")
	}
	return read, nil
}
