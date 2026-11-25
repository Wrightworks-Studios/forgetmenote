package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

// The catalog is one versioned document per org. Each locked section first
// checks the stored version, at most once per freshness window, and reloads a
// changed document; each change writes the document only over the version it
// was computed from, with its activity receipts and logo, in one transaction.
// A change another gateway committed first fails as unavailable, and the next
// call sees it; an activity receipt or first-use account, which decides nothing
// from the catalog, reloads and is written over the stored one. Text Postgres cannot store fails as bad input. While the database is unreachable the catalog grants nothing.
const (
	// Version 10 admits project memory notebooks.
	catalogStorageVersion = 10
	catalogKind           = "catalog"
	catalogID             = "state"
	catalogLimit          = 64 << 20
	// How stale another gateway's committed change may be here.
	catalogFreshness = 250 * time.Millisecond
	// One database call, and so one held catalog lock, never outlasts this.
	catalogTimeout = 10 * time.Second
)

// errCommitUnknown marks a failed commit, which the database may still have
// applied; callers must not discard what such a change published elsewhere.
var errCommitUnknown = errors.New("catalog commit outcome unknown")

func storageHash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func validateActivity(entries []Activity, sequence uint64) error {
	var previous uint64
	for _, entry := range entries {
		if entry.ID == 0 || entry.ID <= previous || previous != 0 && entry.ID != previous+1 {
			return errors.New("invalid catalog activity sequence")
		}
		previous = entry.ID
	}
	if previous != sequence {
		return errors.New("catalog activity sequence does not match its receipts")
	}
	return nil
}

// lock takes c.mu over a current catalog.
func (c *Catalog) lock() {
	c.mu.Lock()
	c.refresh()
}

func (c *Catalog) refresh() {
	if time.Since(c.checked) < c.freshness {
		return
	}
	// A failed check also waits out the window, so an outage never queues
	// every request behind its own database timeout.
	c.checked = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancel()
	if err := c.load(ctx); err != nil {
		log.Printf("catalog unavailable: %v", err)
		c.unavailable()
	}
}

// unavailable leaves an empty catalog that grants nothing and refuses writes
// until a reload succeeds.
func (c *Catalog) unavailable() {
	c.state = catalogData{Users: map[string]User{}, Projects: map[string]Project{}}
	c.initOrganization()
	c.activity, c.logo, c.activityStored, c.logoStored = nil, nil, 0, ""
	c.version = -1
}

// load replaces the catalog with the stored document when its version changed.
func (c *Catalog) load(ctx context.Context) error {
	version, body, err := c.db.ReadDocument(ctx, c.db.Pool, catalogKind, catalogID, c.version)
	if err != nil || version == c.version {
		return err
	}
	var envelope catalogEnvelope
	if version != 0 {
		if err = json.Unmarshal(body, &envelope); err != nil {
			return err
		}
		if envelope.Version != catalogStorageVersion {
			return fmt.Errorf("unsupported catalog version %d", envelope.Version)
		}
	}
	state := envelope.Data
	if len(state.Activity) != 0 || len(state.InstanceLogo) != 0 {
		return errors.New("invalid catalog document")
	}
	if state.Users == nil {
		state.Users = map[string]User{}
	}
	if state.Projects == nil {
		state.Projects = map[string]Project{}
	}
	activity, err := c.loadActivity(ctx, state.ActivitySequence)
	if err != nil {
		return err
	}
	logo := c.logo
	if hash := state.InstanceLogoHash; hash != c.logoStored {
		logo = nil
		if hash != "" {
			if err = c.db.Pool.QueryRow(ctx, "SELECT image FROM catalog_logos WHERE org = $1 AND hash = $2", c.db.Org, hash).Scan(&logo); err != nil || storageHash(logo) != hash {
				return errors.Join(errors.New("catalog logo does not match committed branding"), err)
			}
		}
	}
	c.state, c.activity, c.logo, c.logoStored = state, activity, logo, state.InstanceLogoHash
	c.activityStored = state.ActivitySequence
	if err = c.validate(); err != nil {
		return err
	}
	c.version = version
	return nil
}

// loadActivity returns the newest receipts up to sequence, reading only those
// after the ones already held.
func (c *Catalog) loadActivity(ctx context.Context, sequence uint64) ([]Activity, error) {
	held, from := c.activity, c.activityStored
	if sequence < from || sequence-from > activityLimit {
		held, from = nil, sequence-min(sequence, activityLimit)
	}
	if sequence == from {
		return held, nil
	}
	rows, err := c.db.Pool.Query(ctx, "SELECT entry FROM catalog_activity WHERE org = $1 AND id > $2 AND id <= $3 ORDER BY id", c.db.Org, int64(from), int64(sequence))
	if err != nil {
		return nil, err
	}
	added, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Activity, error) {
		var entry Activity
		err := row.Scan(&entry)
		return entry, err
	})
	if err != nil {
		return nil, err
	}
	entries := append(append([]Activity(nil), held...), added...)
	if len(entries) > activityLimit {
		entries = entries[len(entries)-activityLimit:]
	}
	if err = validateActivity(entries, sequence); err != nil {
		return nil, errors.New("catalog activity does not match committed permissions")
	}
	return entries, nil
}

func (c *Catalog) persist() error {
	if c.version < 0 {
		return ErrStorageUnavailable
	}
	if err := validateActivity(c.activity, c.state.ActivitySequence); err != nil {
		return err
	}
	value := c.state
	value.Activity, value.InstanceLogo, value.InstanceLogoHash = nil, nil, ""
	if len(c.logo) != 0 {
		value.InstanceLogoHash = storageHash(c.logo)
	}
	raw, err := json.Marshal(catalogEnvelope{Version: catalogStorageVersion, Data: value})
	if err != nil {
		return err
	}
	if len(raw) > catalogLimit {
		return errors.New("catalog policy exceeds 64 MiB storage capacity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancel()
	var next int64
	tx, err := c.db.Pool.Begin(ctx)
	if err == nil {
		if next, err = c.write(ctx, tx, raw, value.InstanceLogoHash); err != nil {
			tx.Rollback(ctx)
		} else {
			err = tx.Commit(ctx)
			if err == nil && c.commitFault != nil {
				err = c.commitFault()
			}
			if err != nil {
				err = errors.Join(errCommitUnknown, err)
			}
		}
	}
	if err != nil {
		// Reload before the next decision, whatever the database kept.
		c.version, c.checked = -1, time.Time{}
		if errors.Is(err, pg.ErrNUL) {
			return err
		}
		return errors.Join(ErrStorageUnavailable, err)
	}
	c.version, c.activityStored, c.logoStored, c.checked = next, c.state.ActivitySequence, value.InstanceLogoHash, time.Now()
	return nil
}

// pruneActivity keeps the newest 2*activityLimit receipts up to sequence, as
// the file-based release compacted its log: the catalog reads the newest
// activityLimit and an export writes them all.
func pruneActivity(ctx context.Context, tx pgx.Tx, db *pg.DB, sequence uint64) error {
	if sequence <= 2*activityLimit {
		return nil
	}
	_, err := tx.Exec(ctx, "DELETE FROM catalog_activity WHERE org = $1 AND id <= $2", db.Org, int64(sequence-2*activityLimit))
	return err
}

func (c *Catalog) write(ctx context.Context, tx pgx.Tx, raw []byte, logoHash string) (int64, error) {
	next, err := c.db.WriteDocument(ctx, tx, catalogKind, catalogID, c.version, raw)
	if err != nil {
		return 0, err
	}
	// Receipts are append-only; the catalog reads only the newest.
	added := false
	for _, entry := range c.activity {
		if entry.ID <= c.activityStored {
			continue
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return 0, err
		}
		if len(encoded) >= 1<<20 {
			return 0, errors.New("catalog activity entry exceeds 1 MiB")
		}
		if _, err = tx.Exec(ctx, "INSERT INTO catalog_activity (org, id, entry) VALUES ($1, $2, $3)", c.db.Org, int64(entry.ID), encoded); err != nil {
			return 0, err
		}
		added = true
	}
	if added {
		if err = pruneActivity(ctx, tx, c.db, c.state.ActivitySequence); err != nil {
			return 0, err
		}
	}
	if logoHash != c.logoStored {
		if logoHash != "" {
			if _, err = tx.Exec(ctx, "INSERT INTO catalog_logos (org, hash, image) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING", c.db.Org, logoHash, c.logo); err != nil {
				return 0, err
			}
		}
		if _, err = tx.Exec(ctx, "DELETE FROM catalog_logos WHERE org = $1 AND hash <> $2", c.db.Org, logoHash); err != nil {
			return 0, err
		}
	}
	return next, nil
}
