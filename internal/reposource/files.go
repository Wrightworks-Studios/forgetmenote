package reposource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

// The file-based release kept each notebook's source state in
// repository-sources/<notebook>/state.json, beside its sources' Git caches,
// indexes and retained evidence. Each source's durable files, which blobs.go
// lists, move to blob storage under the same paths; its caches stay behind.
const sourcesDir = "repository-sources"

func stateFile(project string) string { return sourcesDir + "/" + project + "/state.json" }

// The names a source's evidence directories hold.
var evidenceNames = map[string]*regexp.Regexp{
	"refs":      regexp.MustCompile(`^[0-9a-f]{64}\.json$`),
	"blobs":     regexp.MustCompile(`^[0-9a-f]{64}$`),
	"notebooks": regexp.MustCompile(`^[0-9a-f]{40}\.json$`),
}

// ImportFiles copies each notebook's state.json into its source state
// document, checking it as the file-based release does when it opens. Each
// source's index generation, the one index/current.json names, and retained
// evidence go to blob storage, and the document names that generation.
func ImportFiles(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source) (filestate.Sum, error) {
	var tally filestate.Tally
	entries, err := src.Entries(sourcesDir)
	if err != nil {
		return filestate.Sum{}, err
	}
	if len(entries) > 10000 {
		return filestate.Sum{}, fmt.Errorf("%s holds more than 10000 notebooks: %w", sourcesDir, ErrLimit)
	}
	for _, entry := range entries {
		project := entry.Name()
		// The file-based release keeps nothing else here.
		if !entry.IsDir() || !identifier.MatchString(project) {
			return filestate.Sum{}, fmt.Errorf("%s/%s is not a notebook's repository sources; move it out of the data directory, then import", sourcesDir, project)
		}
		raw, err := src.Read(stateFile(project))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return filestate.Sum{}, err
		}
		// That release decoded state.json over an empty state, so a file
		// without a version or sources reads as version 1 with none.
		state := diskState{Version: 1, Sources: map[string]State{}}
		if len(raw) > 4<<20 || json.Unmarshal(raw, &state) != nil {
			return filestate.Sum{}, fmt.Errorf("%s: invalid repository source state", stateFile(project))
		}
		if err = validState(project, state); err != nil {
			return filestate.Sum{}, fmt.Errorf("%s: %w", stateFile(project), err)
		}
		for _, id := range slices.Sorted(maps.Keys(state.Sources)) {
			dir := sourcesDir + "/" + project + "/" + id
			s := state.Sources[id]
			if s.Index.Generation, err = importGeneration(ctx, src, &tally, dir); err != nil {
				return filestate.Sum{}, err
			}
			state.Sources[id] = s
			if err = importEvidence(ctx, src, &tally, dir); err != nil {
				return filestate.Sum{}, err
			}
		}
		body, err := json.Marshal(state)
		if err != nil {
			return filestate.Sum{}, err
		}
		if _, err = db.WriteDocument(ctx, tx, stateKind, project, 0, body); err != nil {
			return filestate.Sum{}, err
		}
		canonical, err := filestate.JSON(raw)
		if err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(stateFile(project), canonical)
	}
	return tally.Sum(), nil
}

// importGeneration imports the index generation that source directory dir's
// index/current.json names, returning its name, or none when the directory
// holds no complete generation. Its other generations are caches.
func importGeneration(ctx context.Context, src *filestate.Source, tally *filestate.Tally, dir string) (string, error) {
	pointer := dir + "/index/current.json"
	raw, err := src.Read(pointer)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var ptr indexPointer
	if len(raw) > 4096 || json.Unmarshal(raw, &ptr) != nil || !identifier.MatchString(ptr.Generation) {
		// The file-based release served no index from it either.
		return "", nil
	}
	generation := dir + "/index/generations/" + ptr.Generation
	entries, err := src.Entries(generation)
	if err != nil {
		return "", err
	}
	present := map[string]bool{}
	for _, entry := range entries {
		present[entry.Name()] = true
	}
	for _, name := range generationFiles {
		if !present[name] && name != "map.json" {
			return "", nil
		}
	}
	for _, name := range generationFiles {
		if present[name] {
			if err = src.ImportBlob(ctx, tally, generation+"/"+name); err != nil {
				return "", err
			}
		}
	}
	canonical, err := filestate.JSON(raw)
	if err != nil {
		return "", err
	}
	tally.Add(pointer, canonical)
	return ptr.Generation, nil
}

// importEvidence imports source directory dir's retained evidence. Names the
// file-based release never wrote there, such as an interrupted write's, stay.
func importEvidence(ctx context.Context, src *filestate.Source, tally *filestate.Tally, dir string) error {
	for _, kind := range slices.Sorted(maps.Keys(evidenceNames)) {
		rel := dir + "/evidence/" + kind
		entries, err := src.Entries(rel)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if evidenceNames[kind].MatchString(entry.Name()) {
				if err = src.ImportBlob(ctx, tally, rel+"/"+entry.Name()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// exportSource writes source s's index generation, with an index/current.json
// naming it, and its retained evidence from blob storage.
func exportSource(ctx context.Context, out *filestate.Target, tally *filestate.Tally, project string, s State) error {
	if out.Blobs == nil {
		return errors.New("exporting repository sources needs the instance's blob storage")
	}
	dir := sourcesDir + "/" + project + "/" + s.ID
	if s.Index.Generation == "" {
		// A state saved before states named their generation: blob storage
		// at the data directory still holds the pointer that names it.
		var ptr indexPointer
		if raw, err := blob.Read(ctx, out.Blobs, dir+"/index/current.json", 4096); err == nil && json.Unmarshal(raw, &ptr) == nil && identifier.MatchString(ptr.Generation) {
			s.Index.Generation = ptr.Generation
		}
	}
	if s.Index.Generation != "" {
		prefix := dir + "/index/generations/" + s.Index.Generation + "/"
		keys, err := out.Blobs.List(ctx, prefix)
		if err != nil {
			return err
		}
		if slices.Contains(keys, prefix+"manifest.json") {
			for _, key := range keys {
				if slices.Contains(generationFiles, strings.TrimPrefix(key, prefix)) {
					if err = out.ExportBlob(ctx, tally, key); err != nil {
						return err
					}
				}
			}
			raw, _ := json.Marshal(indexPointer{s.Index.Generation})
			pointer := dir + "/index/current.json"
			if err = out.Write(pointer, raw); err != nil {
				return err
			}
			canonical, err := filestate.JSON(raw)
			if err != nil {
				return err
			}
			tally.Add(pointer, canonical)
		}
	}
	keys, err := out.Blobs.List(ctx, dir+"/evidence/")
	if err != nil {
		return err
	}
	for _, key := range keys {
		kind, name := path.Split(strings.TrimPrefix(key, dir+"/evidence/"))
		if pattern := evidenceNames[strings.TrimSuffix(kind, "/")]; pattern != nil && pattern.MatchString(name) {
			if err = out.ExportBlob(ctx, tally, key); err != nil {
				return err
			}
		}
	}
	return nil
}

// ExportFiles writes each notebook's source state document as its state.json,
// as the file-based release writes it, with each source's durable files from
// blob storage. Only the document's version and generation names are this
// release's own: handoff fences and releases are the same fields in both.
func ExportFiles(ctx context.Context, db *pg.DB, q pg.Querier, out *filestate.Target) (filestate.Sum, error) {
	var tally filestate.Tally
	projects, err := db.DocumentIDs(ctx, q, stateKind)
	if err != nil {
		return filestate.Sum{}, err
	}
	for _, project := range projects {
		_, body, err := db.ReadDocument(ctx, q, stateKind, project, -1)
		if err != nil {
			return filestate.Sum{}, err
		}
		var state diskState
		if err = json.Unmarshal(body, &state); err != nil {
			return filestate.Sum{}, err
		}
		if !identifier.MatchString(project) {
			return filestate.Sum{}, fmt.Errorf("invalid repository source notebook %q", project)
		}
		if err = validState(project, state); err != nil {
			return filestate.Sum{}, fmt.Errorf("%s: %w", project, err)
		}
		for _, id := range slices.Sorted(maps.Keys(state.Sources)) {
			s := state.Sources[id]
			if err = exportSource(ctx, out, &tally, project, s); err != nil {
				return filestate.Sum{}, fmt.Errorf("%s: %w", project, err)
			}
			// That release finds the generation in index/current.json.
			s.Index.Generation = ""
			state.Sources[id] = s
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return filestate.Sum{}, err
		}
		if len(raw) > 4<<20 {
			return filestate.Sum{}, fmt.Errorf("%s exceeds the file-based release's storage capacity", stateFile(project))
		}
		if err = out.Write(stateFile(project), raw); err != nil {
			return filestate.Sum{}, err
		}
		canonical, err := filestate.JSON(raw)
		if err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(stateFile(project), canonical)
	}
	return tally.Sum(), nil
}
