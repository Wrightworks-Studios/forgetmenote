package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

// The file-based release kept each record as a file of the automations
// directory under the record's name, and the configured jobs in config/jobs.json.
// Prepared Python environments in automations/python stay on the volume: they
// are rebuilt from definitions and hold no control-plane state.
const (
	recordsDir      = "automations"
	definitionsFile = "automations/config/jobs.json"
)

// recordLimit returns the largest record of this name the file-based release
// reads, or 0 for a name it never reads.
func recordLimit(name string) int {
	switch {
	case !validRecordName(name) || strings.Contains(name, "/"):
		return 0
	case strings.HasSuffix(name, ".json"), strings.HasSuffix(name, ".effect"), strings.HasSuffix(name, ".job-state"):
		return capacity.StateBytes.Limit()
	case strings.HasSuffix(name, ".state"):
		// Incident, event cursor, repair and Drive checkpoints, named by digest;
		// each reader applies its own smaller limit, as before.
		return 4 << 20
	}
	return 0
}

// checkRecord checks a record as the file-based release does when it opens the
// queue (runs) or first reads it.
func checkRecord(name string, raw []byte) error {
	limit := recordLimit(name)
	if limit == 0 {
		return fmt.Errorf("%s/%s is not automation state; move it out of the data directory, then import", recordsDir, name)
	}
	if len(raw) > limit {
		return fmt.Errorf("%s/%s exceeds its storage limit", recordsDir, name)
	}
	var err error
	switch {
	case strings.HasSuffix(name, ".json"):
		id := strings.TrimSuffix(name, ".json")
		if !digestPattern.MatchString(id) {
			return fmt.Errorf("%s/%s is not an automation run", recordsDir, name)
		}
		_, err = decodeRun(id, raw)
	case strings.HasSuffix(name, ".effect"):
		err = json.Unmarshal(raw, &Effect{})
	case strings.HasSuffix(name, ".job-state"):
		err = json.Unmarshal(raw, &JobState{})
	default:
		var state struct {
			Version int `json:"version"`
		}
		if err = json.Unmarshal(raw, &state); err == nil && state.Version != 1 {
			err = errors.New("unsupported checkpoint version")
		}
	}
	if err != nil {
		return fmt.Errorf("%s/%s: %w", recordsDir, name, err)
	}
	return nil
}

// ImportFiles copies every record file and jobs.json into automation_records
// and the definitions document, checking them as the file-based release does.
// Running runs are imported as they are: that release queued them again when it
// opened, and Open here does the same for runs whose gateway is gone, which a
// run without a claimant always is.
func ImportFiles(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source) (filestate.Sum, error) {
	var tally filestate.Tally
	entries, err := src.Entries(recordsDir)
	if err != nil {
		return filestate.Sum{}, err
	}
	var rows [][]any
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && (name == "config" || name == "python") {
			continue
		}
		// A write interrupted before its rename leaves this temporary file beside
		// the record it would have replaced; the file-based release never reads it.
		if strings.HasPrefix(name, ".pending-") && entry.Type().IsRegular() {
			continue
		}
		rel := recordsDir + "/" + name
		raw, err := src.Read(rel)
		if err != nil {
			return filestate.Sum{}, err
		}
		if err = checkRecord(name, raw); err != nil {
			return filestate.Sum{}, err
		}
		canonical, err := filestate.JSON(raw)
		if err != nil {
			return filestate.Sum{}, fmt.Errorf("%s: %w", rel, err)
		}
		tally.Add(rel, canonical)
		rows = append(rows, []any{db.Org, name, int64(1), raw})
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"automation_records"}, []string{"org", "name", "version", "body"}, pgx.CopyFromRows(rows)); err != nil {
		return filestate.Sum{}, err
	}
	raw, err := src.Read(definitionsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return tally.Sum(), nil
	}
	if err != nil {
		return filestate.Sum{}, err
	}
	// The file-based release decoded jobs.json over an empty configuration, so a
	// file without a version or jobs reads as version 1 with no jobs.
	state := emptyDefinitions()
	if len(raw) > capacity.StateBytes.Limit() || json.Unmarshal(raw, &state) != nil || state.Version != 1 || state.Jobs == nil {
		return filestate.Sum{}, errors.New("invalid automation definitions")
	}
	if err = validDefinitions(&state); err != nil {
		return filestate.Sum{}, err
	}
	body, err := json.Marshal(state)
	if err != nil {
		return filestate.Sum{}, err
	}
	if _, err = db.WriteDocument(ctx, tx, definitionsKind, definitionsID, 0, body); err != nil {
		return filestate.Sum{}, err
	}
	canonical, err := filestate.JSON(raw)
	if err != nil {
		return filestate.Sum{}, err
	}
	tally.Add(definitionsFile, canonical)
	return tally.Sum(), nil
}

// ExportFiles writes every record under its name and the definitions document
// as jobs.json, as the file-based release writes them. Row and document
// versions are dropped: that release tells changed records apart by file.
func ExportFiles(ctx context.Context, db *pg.DB, q pg.Querier, out *filestate.Target) (filestate.Sum, error) {
	var tally filestate.Tally
	rows, err := q.Query(ctx, `SELECT name, body FROM automation_records WHERE org = $1 ORDER BY name`, db.Org)
	if err != nil {
		return filestate.Sum{}, err
	}
	type record struct {
		name string
		body []byte
	}
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record, error) {
		var r record
		return r, row.Scan(&r.name, &r.body)
	})
	if err != nil {
		return filestate.Sum{}, err
	}
	for _, r := range records {
		raw, err := withoutClaimant(r.name, r.body)
		if err == nil {
			err = checkRecord(r.name, raw)
		}
		if err != nil {
			return filestate.Sum{}, err
		}
		rel := recordsDir + "/" + r.name
		if err = out.Write(rel, raw); err != nil {
			return filestate.Sum{}, err
		}
		canonical, err := filestate.JSON(raw)
		if err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(rel, canonical)
	}
	version, body, err := db.ReadDocument(ctx, q, definitionsKind, definitionsID, -1)
	if err != nil || version == 0 {
		return tally.Sum(), err
	}
	var state definitionState
	if err = json.Unmarshal(body, &state); err != nil {
		return filestate.Sum{}, err
	}
	if err = validDefinitions(&state); err != nil {
		return filestate.Sum{}, err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return filestate.Sum{}, err
	}
	// The document is stored compact; the file-based release refuses an
	// indented file past the same limit.
	if len(raw) > capacity.StateBytes.Limit() {
		return filestate.Sum{}, errors.New("automation definitions exceed the file-based release's storage capacity")
	}
	if err = out.Write(definitionsFile, raw); err != nil {
		return filestate.Sum{}, err
	}
	canonical, err := filestate.JSON(raw)
	if err != nil {
		return filestate.Sum{}, err
	}
	tally.Add(definitionsFile, canonical)
	return tally.Sum(), nil
}

// withoutClaimant drops the gateway a run records while it runs, which the
// file-based release does not know: it queues every running run again when it
// opens, as after its own restart. q.write encodes the claimant last, so the
// rest of the run keeps its bytes.
func withoutClaimant(name string, raw []byte) ([]byte, error) {
	if !strings.HasSuffix(name, ".json") {
		return raw, nil
	}
	var run struct {
		Claimant string `json:"claimant"`
	}
	if err := json.Unmarshal(raw, &run); err != nil || run.Claimant == "" {
		return raw, err
	}
	holder, err := json.Marshal(run.Claimant)
	if err != nil {
		return nil, err
	}
	rest, ok := bytes.CutSuffix(raw, append(append([]byte(",\n  \"claimant\": "), holder...), "\n}"...))
	if !ok {
		return nil, fmt.Errorf("%s/%s does not end with its claimant", recordsDir, name)
	}
	return append(rest, "\n}"...), nil
}
