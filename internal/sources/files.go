package sources

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

// The file-based release kept each job's manifest as source-jobs/<id>/job.json
// beside its original, page checkpoints, result and scratch. Manifests move
// into rows, and the original, checkpoints and result into blob storage under
// the same paths; a data directory that is the blob storage keeps them as
// they are. Scratch stays behind.
const jobsDir = "source-jobs"

func manifestFile(id string) string { return jobsDir + "/" + id + "/job.json" }

// ImportFiles publishes each job's manifest as a row, checking it as the
// file-based release does when it opens, and its files as objects. That
// release's repairs on opening are also OpenJobs's, so the import leaves them
// to it: it removes staging and retired directories and directories without a
// manifest, retires expired jobs, removes scratch and interrupts queued and
// running jobs, whose missing holder is a gateway that is gone. The objects
// are stored before the rows commit, which no running gateway's sweep could
// mistake for leftovers, since none runs during an import.
func ImportFiles(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source) (filestate.Sum, error) {
	var tally filestate.Tally
	entries, err := src.Entries(jobsDir)
	if err != nil {
		return filestate.Sum{}, err
	}
	var rows [][]any
	for _, entry := range entries {
		id := entry.Name()
		if intermediateJobID.MatchString(id) && entry.IsDir() {
			continue
		}
		if !jobID.MatchString(id) || !entry.IsDir() {
			return filestate.Sum{}, fmt.Errorf("invalid source job directory %s/%s", jobsDir, id)
		}
		raw, err := src.Read(manifestFile(id))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return filestate.Sum{}, err
		}
		job, err := decodeJob(id, 1, 0, raw)
		// That release knows no holder, stored count, attempts or staging, so
		// its strict decoding refuses them.
		if err == nil && (job.Holder != "" || job.Stored != 0 || job.Attempt != 0 || job.Fetched != 0 || job.Runs != nil || job.State == "staging") {
			err = errors.New("invalid source job manifest")
		}
		if err != nil {
			return filestate.Sum{}, fmt.Errorf("%s: %w", manifestFile(id), err)
		}
		canonical, err := filestate.JSON(raw)
		if err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(manifestFile(id), canonical)
		files, err := src.Entries(jobsDir + "/" + id)
		if err != nil {
			return filestate.Sum{}, err
		}
		for _, file := range files {
			name, rel := file.Name(), jobsDir+"/"+id+"/"+file.Name()
			switch {
			// Temporaries an interrupted write left, and conversion scratch,
			// stay behind.
			case name == "job.json", strings.HasPrefix(name, "."), name == "scratch" && file.IsDir():
			case jobObject.MatchString(name) && file.Type().IsRegular():
				info, err := file.Info()
				if err == nil {
					err = src.ImportBlob(ctx, &tally, rel)
				}
				if err != nil {
					return filestate.Sum{}, err
				}
				// That release charged a reopened job's actual files.
				job.Stored += info.Size()
			default:
				return filestate.Sum{}, fmt.Errorf("unexpected source job entry %s", rel)
			}
		}
		manifest, err := marshalJob(job)
		if err != nil {
			return filestate.Sum{}, err
		}
		// That release reserved nothing across a restart.
		rows = append(rows, []any{db.Org, id, int64(1), int64(0), manifest})
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"source_jobs"}, []string{"org", "id", "version", "reserved", "manifest"}, pgx.CopyFromRows(rows)); err != nil {
		return filestate.Sum{}, err
	}
	return tally.Sum(), nil
}

// ExportFiles writes each row as its job's job.json beside the job's objects.
// The row version, the holder, the stored count, the attempts and the
// reservation are dropped: the file-based release interrupts queued and
// running jobs and charges their actual files when it opens, as after its own
// restart. Each object the row names is written at the path that release reads
// it at, and the objects stored at those paths that the row does not name are
// written as they are, as the import read them. A job still being created was
// never acknowledged, and is left out.
func ExportFiles(ctx context.Context, db *pg.DB, q pg.Querier, out *filestate.Target) (filestate.Sum, error) {
	var tally filestate.Tally
	if out.Blobs == nil {
		return filestate.Sum{}, errors.New("exporting source jobs requires the instance's blob storage")
	}
	jobs, err := (&Jobs{db: db}).list(ctx, q)
	if err != nil {
		return filestate.Sum{}, err
	}
	for _, job := range jobs {
		if !jobID.MatchString(job.ID) {
			return filestate.Sum{}, fmt.Errorf("invalid source job %q", job.ID)
		}
		if job.State == "staging" {
			continue
		}
		named := map[string]string{"original": job.originalKey()}
		if job.State == "ready" || job.State == "partial" {
			named["result.json"] = job.resultKey()
		}
		for n := 1; n <= job.Pages; n++ {
			named[pageFile(n)] = job.pageKey(n)
		}
		job.Holder, job.Stored, job.Attempt, job.Fetched, job.Runs = "", 0, 0, 0, nil
		raw, err := marshalJob(job)
		if err != nil {
			return filestate.Sum{}, err
		}
		if err = out.Write(manifestFile(job.ID), raw); err != nil {
			return filestate.Sum{}, err
		}
		canonical, err := filestate.JSON(raw)
		if err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(manifestFile(job.ID), canonical)
		dir := jobsDir + "/" + job.ID
		keys, err := out.Blobs.List(ctx, dir+"/")
		if err != nil {
			return filestate.Sum{}, err
		}
		stored, files := map[string]bool{}, map[string]string{}
		for _, key := range keys {
			stored[key] = true
			// A data directory that is the blob storage also holds the
			// manifest an import left and any release's leftovers.
			if path.Dir(key) == dir && jobObject.MatchString(path.Base(key)) {
				files[path.Base(key)] = key
			}
		}
		for name, key := range named {
			if stored[key] {
				files[name] = key
			}
		}
		for _, name := range slices.Sorted(maps.Keys(files)) {
			if err = out.ExportBlobAs(ctx, &tally, files[name], dir+"/"+name); err != nil {
				return filestate.Sum{}, err
			}
		}
	}
	return tally.Sum(), nil
}
