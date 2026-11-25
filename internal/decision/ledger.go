package decision

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"forgetmenote/internal/pg"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const ledgerTimeout = 10 * time.Second

// A full ledger keeps settled attempts from its last ledgerKeepDays days, so an
// operation retried after a restart still finds its attempt; the busiest week
// on record used 12,275 attempts of 20,000. Compaction leaves the ledger at
// ledgerKeepShare of its capacity, so it runs rarely.
const (
	ledgerKeepDays  = 7
	ledgerKeepShare = 0.75
)

// Scope is trusted orchestration metadata, never model-selected. Operation is a
// stable checkpoint identity; retries receive separate attempt numbers.
type Scope struct {
	Actor     string `json:"actor"`
	Notebook  string `json:"notebook"`
	Feature   string `json:"feature"`
	Run       string `json:"run"`
	Chat      string `json:"chat,omitempty"`
	Operation string `json:"operation"`
}
type Record struct {
	ID             string `json:"id"`
	Scope          Scope  `json:"scope"`
	Attempt        int    `json:"attempt"`
	Day            string `json:"day"`
	CreatedAt      string `json:"createdAt"`
	InputHash      string `json:"inputHash"`
	RequestedModel string `json:"requestedModel"`
	ReturnedModel  string `json:"returnedModel,omitempty"`
	Estimate       int64  `json:"estimate"`
	Status         string `json:"status"`
	Usage          *Usage `json:"usage,omitempty"`
}

func (r Record) Tokens() int64 {
	if r.Usage != nil {
		return r.Usage.InputTokens + r.Usage.OutputTokens
	}
	return r.Estimate
}

// Ledger journals paid calls in the instance's database, which every gateway
// shares: an operation's attempt is reserved once, by one ledger, which alone
// settles it.
type Ledger struct {
	mu     sync.Mutex
	db     *pg.DB
	holder string
	closed bool
	now    func() time.Time
}

func OpenLedger(db *pg.DB) (*Ledger, error) {
	if db == nil {
		return nil, ErrStorage
	}
	var holder [16]byte
	rand.Read(holder[:])
	return &Ledger{db: db, holder: hex.EncodeToString(holder[:]), now: time.Now}, nil
}
func validScope(s Scope) bool {
	if !identity(s.Actor, 128) || !identity(s.Notebook, 128) || !identity(s.Run, 128) || !identity(s.Operation, 128) || s.Chat != "" && !identity(s.Chat, 128) {
		return false
	}
	switch s.Feature {
	case "automation-script", "rerank", "citations", "consistency", "extraction", "organization", "quicknotes", "repository-map", "profile-learning", "personal-assistant":
		return true
	}
	return false
}
func validRecord(r Record) bool {
	if !validScope(r.Scope) || r.Attempt < 1 || r.Attempt > 3 || r.ID != recordID(r.Scope, r.Attempt) || !identity(r.RequestedModel, 128) || r.ReturnedModel != "" && !identity(r.ReturnedModel, 128) || r.Estimate < 1 || r.Estimate > 1000000000 || len(r.InputHash) != 64 {
		return false
	}
	if _, err := hex.DecodeString(r.InputHash); err != nil {
		return false
	}
	if _, err := time.Parse("2006-01-02", r.Day); err != nil {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, r.CreatedAt); err != nil {
		return false
	}
	if r.Usage != nil && (r.Usage.InputTokens < 0 || r.Usage.OutputTokens < 0 || r.Usage.InputTokens > 1000000000 || r.Usage.OutputTokens > 1000000000) {
		return false
	}
	return r.Status == "settled" && r.Usage != nil || (r.Status == "reserved" || r.Status == "unknown") && r.Usage == nil
}
func recordID(s Scope, attempt int) string {
	raw, _ := json.Marshal(struct {
		Scope   Scope
		Attempt int
	}{s, attempt})
	return digest(raw)
}
func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func (l *Ledger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
}
func (l *Ledger) open() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.closed
}

// Records returns every attempt in reservation order. An attempt still
// reserved by another ledger, such as one that stopped with a call in flight,
// is unknown here: this ledger can never settle it.
func (l *Ledger) Records() ([]Record, error) {
	if !l.open() {
		return nil, ErrStorage
	}
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	rows, err := l.db.Pool.Query(ctx, `SELECT holder, record::text FROM jev_usage WHERE org = $1 ORDER BY seq`, l.db.Org)
	if err != nil {
		return nil, ErrStorage
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var holder string
		var raw []byte
		var r Record
		if rows.Scan(&holder, &raw) != nil || !strictJSON(raw, &r) || !validRecord(r) {
			return nil, ErrStorage
		}
		if r.Status == "reserved" && holder != l.holder {
			r.Status = "unknown"
		}
		out = append(out, r)
	}
	if rows.Err() != nil {
		return nil, ErrStorage
	}
	return out, nil
}
func (l *Ledger) reserve(scope Scope, attempt int, raw []byte, estimate int64, c Config) (string, error) {
	if !l.open() {
		return "", ErrStorage
	}
	if !validScope(scope) || attempt < 1 || attempt > 3 || estimate < 1 {
		return "", ErrInvalid
	}
	now := l.now().UTC()
	id := recordID(scope, attempt)
	record, err := json.Marshal(Record{ID: id, Scope: scope, Attempt: attempt, Day: now.Format("2006-01-02"), CreatedAt: now.Format(time.RFC3339Nano), InputHash: digest(raw), RequestedModel: c.Model, Estimate: estimate, Status: "reserved"})
	if err != nil {
		return "", ErrStorage
	}
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	for compacted := false; ; compacted = true {
		var exists, inserted bool
		err = l.db.Pool.QueryRow(ctx, `WITH reserved AS (
				INSERT INTO jev_usage (org, id, holder, record)
				SELECT $1, $2, $3, $4::jsonb WHERE (SELECT count(*) FROM jev_usage WHERE org = $1) < $5
				ON CONFLICT DO NOTHING RETURNING id)
			SELECT EXISTS (SELECT 1 FROM reserved), EXISTS (SELECT 1 FROM jev_usage WHERE org = $1 AND id = $2)`,
			l.db.Org, id, l.holder, string(record), c.Limits.LedgerRecords).Scan(&inserted, &exists)
		switch {
		case err != nil:
			return "", ErrStorage
		case inserted:
			return id, nil
		case exists:
			return "", ErrAlreadyAttempted
		case compacted:
			return "", ErrLimit
		}
		if err = l.compact(ctx, now, c.Limits.LedgerRecords); err != nil {
			return "", ErrStorage
		}
	}
}

// compact makes room in a full ledger. Its oldest settled attempts from before
// the last ledgerKeepDays days become daily totals per actor, notebook,
// feature and model, until the ledger is ledgerKeepShare full or no such
// attempt remains. Reserved and unknown attempts always stay, so an uncertain
// call is never purchased again. Gateways compact one at a time.
func (l *Ledger) compact(ctx context.Context, now time.Time, capacity int) error {
	keep := int(float64(capacity) * ledgerKeepShare)
	cutoff := now.AddDate(0, 0, -ledgerKeepDays).Format(time.DateOnly)
	return pgx.BeginFunc(ctx, l.db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('jev-usage ' || $1, 0))`, l.db.Org); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jev_usage WHERE org = $1`, l.db.Org).Scan(&count); err != nil {
			return err
		}
		if count <= keep {
			return nil // Another gateway made room.
		}
		_, err := tx.Exec(ctx, `WITH old AS (
				DELETE FROM jev_usage WHERE org = $1 AND id IN (
					SELECT id FROM jev_usage
					WHERE org = $1 AND record->>'status' = 'settled' AND record->>'day' < $2
					ORDER BY seq LIMIT $3)
				RETURNING record)
			INSERT INTO jev_usage_totals AS t (org, day, feature, actor, notebook, requested_model, returned_model, attempts, input_tokens, output_tokens, estimate)
			SELECT $1, (record->>'day')::date, record->'scope'->>'feature', record->'scope'->>'actor', record->'scope'->>'notebook',
				record->>'requestedModel', COALESCE(record->>'returnedModel', ''), count(*),
				sum((record->'usage'->>'input_tokens')::bigint), sum((record->'usage'->>'output_tokens')::bigint), sum((record->>'estimate')::bigint)
			FROM old GROUP BY 2, 3, 4, 5, 6, 7
			ON CONFLICT (org, day, feature, actor, notebook, requested_model, returned_model) DO UPDATE SET
				attempts = t.attempts + EXCLUDED.attempts,
				input_tokens = t.input_tokens + EXCLUDED.input_tokens,
				output_tokens = t.output_tokens + EXCLUDED.output_tokens,
				estimate = t.estimate + EXCLUDED.estimate`,
			l.db.Org, cutoff, count-keep)
		return err
	})
}
func (l *Ledger) settle(id string, result Result) error {
	if !l.open() {
		return ErrStorage
	}
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	var holder string
	var raw []byte
	err := l.db.Pool.QueryRow(ctx, `SELECT holder, record::text FROM jev_usage WHERE org = $1 AND id = $2`, l.db.Org, id).Scan(&holder, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalid
	}
	var r Record
	if err != nil || !strictJSON(raw, &r) {
		return ErrStorage
	}
	if r.Status != "reserved" || holder != l.holder {
		return ErrAlreadyAttempted
	}
	r.Status = "unknown"
	if result.Usage != nil {
		u := *result.Usage
		r.Usage = &u
		r.Status = "settled"
	}
	if identity(result.Model, 128) {
		r.ReturnedModel = result.Model
	}
	if !validRecord(r) {
		return ErrInvalid
	}
	if raw, err = json.Marshal(r); err != nil {
		return ErrStorage
	}
	tag, err := l.db.Pool.Exec(ctx, `UPDATE jev_usage SET record = $4::jsonb
		WHERE org = $1 AND id = $2 AND holder = $3 AND record->>'status' = 'reserved'`, l.db.Org, id, l.holder, string(raw))
	if err != nil {
		return ErrStorage
	}
	if tag.RowsAffected() != 1 {
		return ErrAlreadyAttempted
	}
	return nil
}

// ReserveExternal extends the existing paid-work journal to source processing.
// A stable operation can be reserved once; restart and unknown usage never make
// it eligible for replay. Inputs are hashed, never retained in the ledger.
func (l *Ledger) ReserveExternal(scope Scope, model string, input []byte, estimate int64) (string, error) {
	if !identity(model, 128) {
		return "", ErrInvalid
	}
	return l.reserve(scope, 1, input, estimate, Config{Model: model, Limits: Limits{LedgerRecords: 20000}})
}
func (l *Ledger) SettleExternal(id string, model string, usage *Usage) error {
	return l.settle(id, Result{Model: model, Usage: usage})
}
