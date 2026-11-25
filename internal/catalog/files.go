package catalog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

// The file-based release kept the catalog in catalog.json, its receipts in
// activity.jsonl and its logo in catalog-logo-<sha256>.bin, beside a journal
// of an interrupted change that it repairs when it opens.
const (
	catalogFile  = "catalog.json"
	activityFile = "activity.jsonl"
	journalFile  = "catalog.transaction.json"
	// Version 9 moved receipts and the logo out of catalog.json; version 10
	// only admits project memory notebooks, which earlier versions never wrote.
	separateStorageVersion = 9
	activityFileLimit      = 64 << 20
)

func logoFile(hash string) string { return "catalog-logo-" + hash + ".bin" }

// fileEpochKey and storedEpochKey convert a per-notebook access epoch key
// between the file-based release's "notebook\x00user", which jsonb refuses, and
// accessKey's "notebook/user". Neither notebook nor user IDs contain either.
func fileEpochKey(key string) string   { return strings.Replace(key, "/", "\x00", 1) }
func storedEpochKey(key string) string { return strings.Replace(key, "\x00", "/", 1) }

func rekeyEpochs(epochs map[string]uint64, rekey func(string) string) map[string]uint64 {
	if epochs == nil {
		return nil
	}
	out := make(map[string]uint64, len(epochs))
	for key, epoch := range epochs {
		out[rekey(key)] = epoch
	}
	return out
}

// ImportFiles copies the catalog, every receipt and the logo into the catalog
// document and its tables, checking them as the file-based release does when
// it opens. It reads every catalog version that release reads, and refuses an
// interrupted change, which only that release repairs.
func ImportFiles(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source) (filestate.Sum, error) {
	var tally filestate.Tally
	if _, err := src.Read(journalFile); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			err = errors.New("catalog.transaction.json records an interrupted change; start the file-based release once so it repairs the catalog, stop it, then import")
		}
		return filestate.Sum{}, err
	}
	raw, err := src.Read(catalogFile)
	if errors.Is(err, fs.ErrNotExist) {
		return tally.Sum(), nil
	}
	if err != nil {
		return filestate.Sum{}, err
	}
	state, version, err := decodeCatalogFile(raw)
	if err != nil {
		return filestate.Sum{}, err
	}
	// Validate a separate decoding: the checks fill missing maps, and the
	// document keeps the file's own.
	probe, _, _ := decodeCatalogFile(raw)
	if err = validateImported(&Catalog{state: probe}, version); err != nil {
		return filestate.Sum{}, err
	}
	var activity []Activity
	var log, logo []byte
	if version < separateStorageVersion {
		// Older layouts keep receipts and the logo inside catalog.json; the
		// file-based release separates them on its next change, and the digests
		// describe those separated files, which an export writes.
		if err = validateActivity(state.Activity, state.ActivitySequence); err != nil {
			return filestate.Sum{}, err
		}
		activity, logo = state.Activity, state.InstanceLogo
		state.Activity, state.InstanceLogo, state.InstanceLogoHash = nil, nil, ""
		if len(logo) != 0 {
			state.InstanceLogoHash = storageHash(logo)
		}
		if len(activity) != 0 {
			if log, err = encodeActivity(activity); err != nil {
				return filestate.Sum{}, err
			}
		}
		if raw, err = json.Marshal(catalogEnvelope{Version: catalogStorageVersion, Data: state}); err != nil {
			return filestate.Sum{}, err
		}
	} else {
		if activity, log, err = readActivityFile(src, state.ActivitySequence); err != nil {
			return filestate.Sum{}, err
		}
		if hash := state.InstanceLogoHash; hash != "" {
			if decoded, err := hex.DecodeString(hash); err != nil || len(decoded) != 32 {
				return filestate.Sum{}, errors.New("invalid catalog logo reference")
			}
			if logo, err = src.Read(logoFile(hash)); err != nil || storageHash(logo) != hash {
				return filestate.Sum{}, errors.Join(errors.New("catalog logo does not match committed branding"), err)
			}
		}
	}
	state.ProjectAccessEpoch = rekeyEpochs(state.ProjectAccessEpoch, storedEpochKey)
	body, err := json.Marshal(catalogEnvelope{Version: catalogStorageVersion, Data: state})
	if err != nil {
		return filestate.Sum{}, err
	}
	if len(body) > catalogLimit {
		return filestate.Sum{}, errors.New("catalog policy exceeds 64 MiB storage capacity")
	}
	if _, err = db.WriteDocument(ctx, tx, catalogKind, catalogID, 0, body); err != nil {
		return filestate.Sum{}, err
	}
	rows := make([][]any, 0, len(activity))
	for _, entry := range activity {
		encoded, err := json.Marshal(entry)
		if err != nil {
			return filestate.Sum{}, err
		}
		if len(encoded) >= 1<<20 {
			return filestate.Sum{}, errors.New("catalog activity entry exceeds 1 MiB")
		}
		rows = append(rows, []any{db.Org, int64(entry.ID), encoded})
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"catalog_activity"}, []string{"org", "id", "entry"}, pgx.CopyFromRows(rows)); err != nil {
		return filestate.Sum{}, err
	}
	if len(logo) != 0 {
		if _, err = tx.Exec(ctx, "INSERT INTO catalog_logos (org, hash, image) VALUES ($1, $2, $3)", db.Org, state.InstanceLogoHash, logo); err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(logoFile(state.InstanceLogoHash), logo)
	}
	// An older catalog reads as version 10, the form an export writes.
	canonical, err := canonicalCatalog(raw)
	if err != nil {
		return filestate.Sum{}, err
	}
	tally.Add(catalogFile, canonical)
	if log != nil {
		if canonical, err = canonicalActivity(log); err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(activityFile, canonical)
	}
	return tally.Sum(), nil
}

// ExportFiles writes the catalog document as a version 10 catalog.json with the
// newest receipts the file-based release keeps and the current logo.
func ExportFiles(ctx context.Context, db *pg.DB, q pg.Querier, out *filestate.Target) (filestate.Sum, error) {
	var tally filestate.Tally
	version, body, err := db.ReadDocument(ctx, q, catalogKind, catalogID, -1)
	if err != nil || version == 0 {
		return tally.Sum(), err
	}
	var envelope catalogEnvelope
	if err = json.Unmarshal(body, &envelope); err != nil {
		return filestate.Sum{}, err
	}
	if envelope.Version != catalogStorageVersion {
		return filestate.Sum{}, fmt.Errorf("unsupported catalog version %d", envelope.Version)
	}
	state := envelope.Data
	state.ProjectAccessEpoch = rekeyEpochs(state.ProjectAccessEpoch, fileEpochKey)
	// It compacts its log to the newest receipts before it reaches twice as
	// many as it reads, so the export holds at most that many.
	rows, err := q.Query(ctx, `SELECT entry FROM (SELECT id, entry FROM catalog_activity WHERE org = $1 AND id <= $2 ORDER BY id DESC LIMIT $3) newest ORDER BY id`,
		db.Org, int64(state.ActivitySequence), 2*activityLimit)
	if err != nil {
		return filestate.Sum{}, err
	}
	activity, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Activity, error) {
		var entry Activity
		err := row.Scan(&entry)
		return entry, err
	})
	if err != nil {
		return filestate.Sum{}, err
	}
	if err = validateActivity(activity, state.ActivitySequence); err != nil {
		return filestate.Sum{}, err
	}
	if hash := state.InstanceLogoHash; hash != "" {
		var logo []byte
		if err = q.QueryRow(ctx, "SELECT image FROM catalog_logos WHERE org = $1 AND hash = $2", db.Org, hash).Scan(&logo); err != nil || storageHash(logo) != hash {
			return filestate.Sum{}, errors.Join(errors.New("catalog logo does not match committed branding"), err)
		}
		if err = out.Write(logoFile(hash), logo); err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(logoFile(hash), logo)
	}
	if len(activity) > 0 {
		log, err := encodeActivity(activity)
		if err != nil {
			return filestate.Sum{}, err
		}
		if err = out.Write(activityFile, log); err != nil {
			return filestate.Sum{}, err
		}
		canonical, err := canonicalActivity(log)
		if err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(activityFile, canonical)
	}
	raw, err := json.MarshalIndent(catalogEnvelope{Version: catalogStorageVersion, Data: state}, "", "  ")
	if err != nil {
		return filestate.Sum{}, err
	}
	if err = out.Write(catalogFile, raw); err != nil {
		return filestate.Sum{}, err
	}
	canonical, err := canonicalCatalog(raw)
	if err != nil {
		return filestate.Sum{}, err
	}
	tally.Add(catalogFile, canonical)
	return tally.Sum(), nil
}

// decodeCatalogFile reads catalog.json in any layout the file-based release
// reads: unversioned, or a versioned envelope.
func decodeCatalogFile(raw []byte) (catalogData, int, error) {
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return catalogData{}, 0, errors.New("cannot read project catalog")
	}
	var state catalogData
	var err error
	switch header.Version {
	case 0:
		err = json.Unmarshal(raw, &state)
	case 2, 3, 4, 5, 6, 7, 8, separateStorageVersion, catalogStorageVersion:
		var envelope catalogEnvelope
		err = json.Unmarshal(raw, &envelope)
		state = envelope.Data
	default:
		return catalogData{}, 0, errors.New("unsupported catalog version")
	}
	if err != nil || state.Users == nil || state.Projects == nil {
		return catalogData{}, 0, errors.New("cannot read project catalog")
	}
	if header.Version >= separateStorageVersion && (len(state.Activity) != 0 || len(state.InstanceLogo) != 0) {
		return catalogData{}, 0, errors.New("invalid separated catalog storage")
	}
	return state, header.Version, nil
}

// validateImported runs the checks the file-based release ran on open that do
// not need the configured owners or the notebooks themselves; a gateway checks
// those when it loads the document.
func validateImported(c *Catalog, version int) error {
	if ai := c.state.AI; ai != nil && (version < 6 || ai.Revision == 0 || ai.validate() != nil) {
		return errors.New("invalid instance AI policy")
	}
	if clients := c.state.ChatClients; clients != nil && (version < 7 || clients.Revision == 0 || clients.validate() != nil) {
		return errors.New("invalid chat client policy")
	}
	if err := c.initOrganization(); err != nil {
		return err
	}
	for _, a := range c.state.Accounts {
		if len(a.ID) != 32 {
			return errors.New("invalid account identity")
		}
	}
	for id, p := range c.state.Projects {
		if !projectID.MatchString(id) || p.ID != id || p.Kind != "" && p.Kind != "notes" && p.Kind != "repository" {
			return errors.New("invalid project catalog")
		}
	}
	for key := range c.state.ProjectAccessEpoch {
		if project, user, ok := strings.Cut(key, "\x00"); !ok || !projectID.MatchString(project) || user == "" || strings.ContainsAny(user, "/\x00") {
			return errors.New("invalid notebook access epoch")
		}
	}
	return errors.Join(c.validateInboxes(), c.validateAssistants(), c.validateAuthorityTransfers())
}

// readActivityFile reads every receipt in activity.jsonl as the file-based
// release does: consecutive, none past sequence, and ending at it.
func readActivityFile(src *filestate.Source, sequence uint64) ([]Activity, []byte, error) {
	raw, err := src.Read(activityFile)
	if errors.Is(err, fs.ErrNotExist) && sequence == 0 {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > activityFileLimit {
		return nil, nil, errors.New("invalid catalog storage file: activity.jsonl")
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var entries []Activity
	var previous uint64
	for scanner.Scan() {
		var entry Activity
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil || entry.ID == 0 || entry.ID > sequence || previous != 0 && entry.ID != previous+1 {
			return nil, nil, errors.New("invalid catalog activity log")
		}
		previous = entry.ID
		entries = append(entries, entry)
	}
	if err = scanner.Err(); err != nil {
		return nil, nil, err
	}
	if sequence != 0 && previous != sequence {
		return nil, nil, errors.New("catalog activity log does not match committed permissions")
	}
	return entries, raw, nil
}

func canonicalCatalog(raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields["version"] = json.RawMessage(fmt.Sprint(catalogStorageVersion))
	versioned, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return filestate.JSON(versioned)
}

// encodeActivity writes receipts one JSON line each, as the file-based
// release's log holds them.
func encodeActivity(entries []Activity) ([]byte, error) {
	var log bytes.Buffer
	encoder := json.NewEncoder(&log)
	for _, entry := range entries {
		before := log.Len()
		if err := encoder.Encode(entry); err != nil {
			return nil, err
		}
		if log.Len()-before >= 1<<20 {
			return nil, errors.New("catalog activity entry exceeds 1 MiB")
		}
	}
	if log.Len() > activityFileLimit {
		return nil, errors.New("catalog activity exceeds the file-based release's storage capacity")
	}
	return log.Bytes(), nil
}

// canonicalActivity is a receipt log line by line in canonical JSON.
func canonicalActivity(raw []byte) ([]byte, error) {
	var lines [][]byte
	for line := range bytes.Lines(raw) {
		canonical, err := filestate.JSON(line)
		if err != nil {
			return nil, err
		}
		lines = append(lines, canonical)
	}
	return bytes.Join(lines, []byte("\n")), nil
}
