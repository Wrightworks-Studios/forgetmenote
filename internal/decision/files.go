package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

// The file-based release kept judgment state under jev/: the paid-call ledger in
// usage.json beside usage.lock, which holds no state, and each named record at
// jev/<kind>/<name>.json. A file with a temporary prefix is a write that
// release never renamed into place and never reads.
const (
	jevDir        = "jev"
	usageFile     = "usage.json"
	usageLock     = "usage.lock"
	usageTemp     = ".usage-"
	recordTemp    = ".jev-"
	ledgerBytes   = 16 << 20
	ledgerRecords = 20000
)

var recordKinds = []string{"citations", "jobs", "notebooks", "notifications", "results", "users"}

var errLedgerFile = errors.New("invalid judgments usage ledger")

type ledgerFile struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}

// ImportFiles copies the ledger and every named record into their rows,
// checking each file as the file-based release does when it reads it. Any
// other file is refused, as that release would never read it.
func ImportFiles(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source) (filestate.Sum, error) {
	var tally filestate.Tally
	entries, err := src.Entries(jevDir)
	if err != nil {
		return filestate.Sum{}, err
	}
	var usage, records [][]any
	for _, entry := range entries {
		rel := jevDir + "/" + entry.Name()
		switch {
		case entry.Name() == usageLock && entry.Type().IsRegular(), strings.HasPrefix(entry.Name(), usageTemp):
			// Neither holds state.
		case entry.Name() == usageFile:
			raw, err := src.Read(rel)
			if err != nil {
				return filestate.Sum{}, err
			}
			if usage, err = importLedger(db, raw); err != nil {
				return filestate.Sum{}, fmt.Errorf("%s: %w", rel, err)
			}
			canonical, err := filestate.JSON(raw)
			if err != nil {
				return filestate.Sum{}, err
			}
			tally.Add(rel, canonical)
		case entry.IsDir() && slices.Contains(recordKinds, entry.Name()):
			files, err := src.Entries(rel)
			if err != nil {
				return filestate.Sum{}, err
			}
			for _, file := range files {
				if strings.HasPrefix(file.Name(), recordTemp) {
					continue
				}
				name := entry.Name() + "/" + file.Name()
				if !strings.HasSuffix(name, ".json") || !recordName(name) {
					return filestate.Sum{}, fmt.Errorf("%s/%s is not judgment state the file-based release reads", jevDir, name)
				}
				raw, err := src.Read(jevDir + "/" + name)
				if err != nil {
					return filestate.Sum{}, err
				}
				if len(raw) > recordBytes || !DecodeRecord(raw, new(json.RawMessage)) {
					return filestate.Sum{}, fmt.Errorf("%s/%s: %w", jevDir, name, ErrStorage)
				}
				canonical, err := canonicalRecord(raw)
				if err != nil {
					return filestate.Sum{}, err
				}
				records = append(records, []any{db.Org, name, raw})
				tally.Add(jevDir+"/"+name, canonical)
			}
		default:
			return filestate.Sum{}, fmt.Errorf("%s is not judgment state the file-based release reads", rel)
		}
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"jev_usage"}, []string{"org", "id", "holder", "record"}, pgx.CopyFromRows(usage)); err != nil {
		return filestate.Sum{}, err
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"jev_records"}, []string{"org", "name", "body"}, pgx.CopyFromRows(records)); err != nil {
		return filestate.Sum{}, err
	}
	return tally.Sum(), nil
}

// importLedger checks usage.json as the file-based release does when it opens
// the ledger, and returns its rows in reservation order. That release marks a
// call still reserved as unknown and rewrites the file when it opens, so an
// import refuses one rather than repair it.
func importLedger(db *pg.DB, raw []byte) ([][]any, error) {
	var envelope struct {
		Version *int               `json:"version"`
		Records *[]json.RawMessage `json:"records"`
	}
	if len(raw) > ledgerBytes || !strictJSON(raw, &envelope) || envelope.Version == nil || *envelope.Version != 1 || envelope.Records == nil || len(*envelope.Records) > ledgerRecords {
		return nil, errLedgerFile
	}
	rows := make([][]any, 0, len(*envelope.Records))
	seen := map[string]bool{}
	for _, rawRecord := range *envelope.Records {
		var r Record
		if !strictJSON(rawRecord, &r) || !validRecord(r) || seen[r.ID] {
			return nil, errLedgerFile
		}
		seen[r.ID] = true
		if r.Status == "reserved" {
			return nil, errors.New("a paid call is still reserved; start the file-based release once so it records the call as unknown, stop it, then import")
		}
		body, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		// No ledger reserved an imported attempt, so none settles it.
		rows = append(rows, []any{db.Org, r.ID, "", body})
	}
	return rows, nil
}

// ExportFiles writes the ledger as usage.json, in reservation order, and every
// named record as its file under jev/. The holders of attempts name ledgers of
// gateways and are left behind.
func ExportFiles(ctx context.Context, db *pg.DB, q pg.Querier, out *filestate.Target) (filestate.Sum, error) {
	var tally filestate.Tally
	rows, err := q.Query(ctx, `SELECT record::text FROM jev_usage WHERE org = $1 ORDER BY seq`, db.Org)
	if err != nil {
		return filestate.Sum{}, err
	}
	usage, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return filestate.Sum{}, err
	}
	if len(usage) > ledgerRecords {
		return filestate.Sum{}, errors.New("the judgments ledger exceeds the file-based release's capacity")
	}
	if len(usage) > 0 {
		ledger := ledgerFile{Version: 1, Records: make([]Record, 0, len(usage))}
		for _, raw := range usage {
			var r Record
			if !strictJSON(raw, &r) || !validRecord(r) {
				return filestate.Sum{}, errLedgerFile
			}
			// Every gateway has stopped, so no ledger can settle a call still
			// reserved. The file-based release would record it as unknown
			// when it opens, as a gateway's ledger reports another's.
			if r.Status == "reserved" {
				r.Status = "unknown"
			}
			ledger.Records = append(ledger.Records, r)
		}
		raw, err := json.Marshal(ledger)
		if err != nil {
			return filestate.Sum{}, err
		}
		if len(raw) > ledgerBytes {
			return filestate.Sum{}, errors.New("the judgments ledger exceeds the file-based release's capacity")
		}
		if err = out.Write(jevDir+"/"+usageFile, raw); err != nil {
			return filestate.Sum{}, err
		}
		canonical, err := filestate.JSON(raw)
		if err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(jevDir+"/"+usageFile, canonical)
	}
	if rows, err = q.Query(ctx, `SELECT name, body::text FROM jev_records WHERE org = $1 ORDER BY name`, db.Org); err != nil {
		return filestate.Sum{}, err
	}
	records, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Name, Body string }])
	if err != nil {
		return filestate.Sum{}, err
	}
	for _, record := range records {
		kind, file, _ := strings.Cut(record.Name, "/")
		if !slices.Contains(recordKinds, kind) || strings.Contains(file, "/") || !strings.HasSuffix(file, ".json") || strings.HasPrefix(file, recordTemp) || !recordName(record.Name) {
			return filestate.Sum{}, fmt.Errorf("judgment record %q has no file in the file-based release", record.Name)
		}
		// jsonb keeps neither key order nor number spelling; the record is
		// written with sorted keys and the numbers' exact decimal values.
		raw, err := filestate.JSON([]byte(record.Body))
		if err != nil {
			return filestate.Sum{}, err
		}
		if len(raw) > recordBytes {
			return filestate.Sum{}, fmt.Errorf("judgment record %q exceeds the file-based release's capacity", record.Name)
		}
		if err = out.Write(jevDir+"/"+record.Name, raw); err != nil {
			return filestate.Sum{}, err
		}
		canonical, err := canonicalRecord(raw)
		if err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(jevDir+"/"+record.Name, canonical)
	}
	return tally.Sum(), nil
}

// canonicalRecord is a record with sorted keys and each number spelled as its
// exact decimal value, so the 1.2e-7 the file-based release wrote and the
// 0.00000012 jsonb returns are the same score, and no two values match.
func canonicalRecord(raw []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	value, err := exactNumbers(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func exactNumbers(value any) (any, error) {
	var err error
	switch v := value.(type) {
	case json.Number:
		return exactDecimal(v)
	case map[string]any:
		for key, item := range v {
			if v[key], err = exactNumbers(item); err != nil {
				return nil, err
			}
		}
	case []any:
		for i, item := range v {
			if v[i], err = exactNumbers(item); err != nil {
				return nil, err
			}
		}
	}
	return value, nil
}

// exactDecimal spells n without an exponent, leading or trailing zeros or a
// negative zero. Exponents past 400 are refused: no record type holds them.
func exactDecimal(n json.Number) (json.Number, error) {
	text := string(n)
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(strings.TrimPrefix(text[i+1:], "+"))
		if err != nil || exponent < -400 || exponent > 400 {
			return "", fmt.Errorf("number %.40s is out of range", text)
		}
	}
	var r big.Rat
	if _, ok := r.SetString(text); !ok {
		return "", fmt.Errorf("invalid number %.40s", text)
	}
	// A JSON number is an integer over a power of ten, so its decimal ends.
	places := 0
	for scaled := new(big.Rat).Set(&r); !scaled.IsInt(); places++ {
		scaled.Mul(scaled, big.NewRat(10, 1))
	}
	return json.Number(r.FloatString(places)), nil
}
