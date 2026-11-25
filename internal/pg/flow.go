package pg

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrFlowsFull reports that a flow group already holds its bound of live flows.
var ErrFlowsFull = errors.New("too many pending connections")

// ErrNoFlow reports a flow that does not exist or has expired.
var ErrNoFlow = errors.New("pending flow not found or expired")

func flowGroup(kind string) string {
	group, _, _ := strings.Cut(kind, "/")
	return group
}

// A FlowBound limits the live flows of a group, the part of a kind before its
// first '/'.
type FlowBound struct {
	Limit int
	// Replace first removes the subject's earlier flows in the group, so one
	// subject holds at most one at a time.
	Replace bool
	// Evict drops the group's soonest-expiring flow when it is full instead of
	// refusing, so a flood of anonymous starts cannot refuse new ones.
	Evict bool
}

// BeginFlow stores a new pending flow of kind under key for subject until ttl
// passes. It removes the group's expired flows, applies bound, and refuses with
// ErrFlowsFull when the group still holds bound.Limit live flows.
func (db *DB) BeginFlow(ctx context.Context, kind, key, subject string, body []byte, ttl time.Duration, bound FlowBound) error {
	return pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		group := flowGroup(kind)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pending-flows ' || $1 || ' ' || $2, 0))`, db.Org, group); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM pending_flows WHERE org = $1 AND split_part(kind, '/', 1) = $2 AND (expires <= now() OR $3 AND subject = $4)`, db.Org, group, bound.Replace, subject); err != nil {
			return err
		}
		var live int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pending_flows WHERE org = $1 AND split_part(kind, '/', 1) = $2`, db.Org, group).Scan(&live); err != nil {
			return err
		}
		if live >= bound.Limit && !bound.Evict {
			return ErrFlowsFull
		}
		if live >= bound.Limit {
			if _, err := tx.Exec(ctx, `DELETE FROM pending_flows WHERE (org, kind, key) IN (
				SELECT org, kind, key FROM pending_flows WHERE org = $1 AND split_part(kind, '/', 1) = $2 ORDER BY expires LIMIT $3)`, db.Org, group, live-bound.Limit+1); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO pending_flows (org, kind, key, subject, expires, body) VALUES ($1, $2, $3, $4, now() + $5 * interval '1 millisecond', $6)`,
			db.Org, kind, key, subject, millis(ttl), body)
		return err
	})
}

// PutFlow stores or replaces a flow of kind under key until expires, as a
// callback does when it advances its flow to the next step.
func (db *DB) PutFlow(ctx context.Context, kind, key, subject string, body []byte, expires time.Time) error {
	_, err := db.Pool.Exec(ctx, `INSERT INTO pending_flows (org, kind, key, subject, expires, body) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (org, kind, key) DO UPDATE SET subject = EXCLUDED.subject, expires = EXCLUDED.expires, body = EXCLUDED.body`,
		db.Org, kind, key, subject, expires, body)
	return err
}

// TakeFlow removes and returns a live flow, so its callback succeeds once. An
// expired flow is removed and reported as ErrNoFlow.
func (db *DB) TakeFlow(ctx context.Context, kind, key string) ([]byte, error) {
	var body []byte
	var live bool
	err := db.Pool.QueryRow(ctx, `DELETE FROM pending_flows WHERE org = $1 AND kind = $2 AND key = $3 RETURNING body, expires > now()`, db.Org, kind, key).Scan(&body, &live)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !live {
		return nil, ErrNoFlow
	}
	return body, err
}

// ReadFlow returns a live flow without removing it. An expired flow is
// removed and reported as ErrNoFlow.
func (db *DB) ReadFlow(ctx context.Context, kind, key string) ([]byte, error) {
	var body []byte
	var live bool
	err := db.Pool.QueryRow(ctx, `SELECT body, expires > now() FROM pending_flows WHERE org = $1 AND kind = $2 AND key = $3`, db.Org, kind, key).Scan(&body, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoFlow
	}
	if err != nil {
		return nil, err
	}
	if !live {
		if _, err := db.Pool.Exec(ctx, `DELETE FROM pending_flows WHERE org = $1 AND kind = $2 AND key = $3 AND expires <= now()`, db.Org, kind, key); err != nil {
			return nil, err
		}
		return nil, ErrNoFlow
	}
	return body, nil
}

// UpdateFlow replaces a live flow's body with change's result while holding
// the row, so concurrent steps on any gateway apply one at a time. An error
// from change leaves the flow unchanged.
func (db *DB) UpdateFlow(ctx context.Context, kind, key string, change func([]byte) ([]byte, error)) error {
	return pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		var body []byte
		err := tx.QueryRow(ctx, `SELECT body FROM pending_flows WHERE org = $1 AND kind = $2 AND key = $3 AND expires > now() FOR UPDATE`, db.Org, kind, key).Scan(&body)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoFlow
		}
		if err != nil {
			return err
		}
		if body, err = change(body); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE pending_flows SET body = $4 WHERE org = $1 AND kind = $2 AND key = $3`, db.Org, kind, key, body)
		return err
	})
}

// SweepFlows removes the org's expired flows, of every kind, and returns
// how many. BeginFlow removes only its own group's, so a flow nobody
// finishes in a group nobody begins another in would otherwise stay, with
// whatever its body holds.
func (db *DB) SweepFlows(ctx context.Context) (int64, error) {
	tag, err := db.Pool.Exec(ctx, `DELETE FROM pending_flows WHERE org = $1 AND expires <= now()`, db.Org)
	return tag.RowsAffected(), err
}

// EndFlows removes the flows of kind that subject holds.
func (db *DB) EndFlows(ctx context.Context, kind, subject string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM pending_flows WHERE org = $1 AND kind = $2 AND subject = $3`, db.Org, kind, subject)
	return err
}

func windowKeys(group string, keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = group + "/" + k
	}
	return out
}

// Take charges one request to each key's one-minute window in group, or
// refuses without charging when any window is at its limit or the group
// already holds capacity live windows. Every gateway shares the windows, so a
// limit holds for the instance.
func (db *DB) Take(ctx context.Context, group string, keys []string, limits []int, capacity int) (bool, error) {
	keys = windowKeys(group, keys)
	err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('rate-windows ' || $1 || ' ' || $2, 0))`, db.Org, group); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM rate_windows WHERE org = $1 AND split_part(key, '/', 1) = $2 AND started <= now() - interval '1 minute'`, db.Org, group); err != nil {
			return err
		}
		var live, fresh int
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE key = ANY($3)) FROM rate_windows WHERE org = $1 AND split_part(key, '/', 1) = $2`, db.Org, group, keys).Scan(&live, &fresh); err != nil {
			return err
		}
		if live-fresh+len(keys) > capacity {
			return errRefused
		}
		if _, err := tx.Exec(ctx, `INSERT INTO rate_windows (org, key, started, count) SELECT $1, k, now(), 0 FROM unnest($2::text[]) k ON CONFLICT DO NOTHING`, db.Org, keys); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT key, count FROM rate_windows WHERE org = $1 AND key = ANY($2)`, db.Org, keys)
		if err != nil {
			return err
		}
		counts := map[string]int{}
		for rows.Next() {
			var key string
			var count int
			if err = rows.Scan(&key, &count); err != nil {
				rows.Close()
				return err
			}
			counts[key] = count
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		for i, key := range keys {
			if counts[key] >= limits[i] {
				return errRefused
			}
		}
		_, err = tx.Exec(ctx, `UPDATE rate_windows SET count = count + 1 WHERE org = $1 AND key = ANY($2)`, db.Org, keys)
		return err
	})
	if errors.Is(err, errRefused) {
		return false, nil
	}
	return err == nil, err
}

var errRefused = errors.New("rate window is full")

// Refund returns one charge to each of group's keys in its current window.
func (db *DB) Refund(ctx context.Context, group string, keys []string) error {
	_, err := db.Pool.Exec(ctx, `UPDATE rate_windows SET count = count - 1 WHERE org = $1 AND key = ANY($2) AND count > 0 AND started > now() - interval '1 minute'`, db.Org, windowKeys(group, keys))
	return err
}
