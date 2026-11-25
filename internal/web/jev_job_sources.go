package web

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"net/url"
	"sort"
	"strings"
)

func jevActiveScope() workspace.ContentScope {
	return workspace.ContentScope{NotesOnly: true, Collection: "notes", Policy: workspace.ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}}
}

func jevInventory(snapshot workspace.Snapshot) string {
	values := []string{snapshot.Visibility, snapshot.ScopeID}
	for _, file := range snapshot.Files {
		values = append(values, file.Path+"\x00"+file.Hash)
	}
	raw, _ := json.Marshal(values)
	return workspace.ContentHash(raw)
}

// jevPassageRunes is the passage size reviews split notes into.
const jevPassageRunes = 1024

func buildJevJob(ctx context.Context, store *workspace.Store, input jevJobInput, changed, known map[string]bool) (jevJob, error) {
	job := jevJob{Notes: []jevJobNote{}, Work: []jevJobWork{}}
	snapshot, err := store.SnapshotScoped(ctx, "", jevActiveScope())
	if err != nil {
		return job, err
	}
	job.Inventory = jevInventory(snapshot)
	current := map[string]workspace.SnapshotFile{}
	for _, file := range snapshot.Files {
		current[file.Path] = file
	}
	sources := map[string]bool{}
	destinations := map[string]bool{}
	for _, path := range input.Destinations {
		if _, ok := current[path]; !ok {
			return job, workspace.ErrForbidden
		}
		destinations[path] = true
	}
	if input.Kind == "audit" && (len(input.Sources) == 0 || len(input.Destinations) == 0) {
		return job, decision.ErrInvalid
	}
	if input.Kind != "audit" && len(input.Destinations) > 0 {
		return job, decision.ErrInvalid
	}
	selectedSnapshot := workspace.Snapshot{Revision: snapshot.Revision, Files: []workspace.SnapshotFile{}}
	for _, file := range snapshot.Files {
		destination := input.Kind != "audit" || destinations[file.Path]
		if destination {
			selectedSnapshot.Files = append(selectedSnapshot.Files, file)
			job.Notes = append(job.Notes, jevJobNote{Path: file.Path, Revision: snapshot.Revision, Hash: file.Hash, CurrentHash: file.Hash, Destination: true})
		}
	}
	passages, err := knowledge.Split(selectedSnapshot, knowledge.Config{PassageRunes: jevPassageRunes})
	if err != nil {
		return job, err
	}
	auditDestinations := passages
	auditSources := []knowledge.Passage{}
	selectedVersions := map[string]bool{}
	for _, selection := range input.Sources {
		if err := ctx.Err(); err != nil {
			return job, err
		}
		if selection.Path == "AGENTS.md" || workspace.IsTrash(selection.Path) {
			return job, workspace.ErrForbidden
		}
		now, err := store.KnowledgeRead(ctx, selection.Path, "")
		if err != nil {
			return job, err
		}
		metadata, err := workspace.ParseMetadata(now.Content)
		if err != nil || metadata.ContainerError != "" || metadata.Container == "draft" || metadata.Container != "" && metadata.Container != "archive" {
			return job, workspace.ErrForbidden
		}
		if input.Kind != "audit" {
			if selection.Revision != "" && selection.Revision != snapshot.Revision {
				return job, workspace.ErrForbidden
			}
			if _, ok := current[selection.Path]; !ok {
				return job, workspace.ErrForbidden
			}
			sources[selection.Path] = true
			continue
		}
		revision := selection.Revision
		if revision == "" {
			revision = now.Revision
		}
		identity := selection.Path + "\x00" + revision
		if selectedVersions[identity] {
			continue
		}
		doc, err := store.KnowledgeRead(ctx, selection.Path, revision)
		if err != nil {
			return job, err
		}
		identity = doc.Path + "\x00" + doc.Revision
		if selectedVersions[identity] {
			continue
		}
		selectedVersions[identity] = true
		oldMeta, err := workspace.ParseMetadata(doc.Content)
		if err != nil || oldMeta.ContainerError != "" || oldMeta.Container == "draft" || oldMeta.Container != "" && oldMeta.Container != "archive" {
			return job, workspace.ErrForbidden
		}
		hash := workspace.ContentHash(doc.Content)
		job.Notes = append(job.Notes, jevJobNote{Path: doc.Path, Revision: doc.Revision, Hash: hash, CurrentHash: workspace.ContentHash(now.Content), Source: true, Historical: doc.Revision != snapshot.Revision, Container: oldMeta.Container})
		split, err := knowledge.Split(workspace.Snapshot{Revision: doc.Revision, Files: []workspace.SnapshotFile{{Path: doc.Path, Content: string(doc.Content), Hash: hash, Metadata: oldMeta}}}, knowledge.Config{PassageRunes: jevPassageRunes})
		if err != nil {
			return job, err
		}
		// Prefix only work identities, never source text, when the same path's older
		// version is compared with its current destination.
		for i := range split {
			split[i].ID = "source:" + split[i].Revision + ":" + split[i].ID
		}
		auditSources = append(auditSources, split...)
		if err := preflightJevAudit(ctx, job, auditSources, auditDestinations); err != nil {
			return job, err
		}
		passages = append(passages, split...)
	}
	if input.Kind == "audit" {
		destinationPassages := []knowledge.Passage{}
		sourcePassages := []knowledge.Passage{}
		for _, passage := range passages {
			if strings.HasPrefix(passage.ID, "source:") {
				sourcePassages = append(sourcePassages, passage)
			} else {
				destinationPassages = append(destinationPassages, passage)
			}
		}
		if err := preflightJevAudit(ctx, job, sourcePassages, destinationPassages); err != nil {
			return job, err
		}
		candidates, err := knowledge.NewReviewCandidates(ctx, destinationPassages)
		if err != nil {
			return job, err
		}
		for _, source := range sourcePassages {
			if err := ctx.Err(); err != nil {
				return job, err
			}
			ordered, err := candidates.Related(ctx, source, len(destinationPassages), true)
			if err != nil {
				return job, err
			}
			if len(ordered) == 0 {
				job.Work = append(job.Work, jevJobWork{Kind: "audit", Passages: []string{source.ID}})
			}
			for start := 0; start < len(ordered); start += 2 {
				if err := ctx.Err(); err != nil {
					return job, err
				}
				refs := []string{source.ID}
				for _, candidate := range ordered[start:min(start+2, len(ordered))] {
					refs = append(refs, candidate.ID)
				}
				job.Work = append(job.Work, jevJobWork{Kind: "audit", Passages: refs})
			}
		}
	} else {
		selected := func(path string) bool {
			return (len(sources) == 0 || sources[path]) && (changed == nil || changed[path])
		}
		candidates, err := knowledge.NewReviewCandidates(ctx, passages)
		if err != nil {
			return job, err
		}
		seen := map[string]bool{}
		for _, left := range passages {
			// A passage whose text an earlier review covered is not checked again.
			if !selected(left.Path) || known[left.Hash] {
				continue
			}
			if input.Kind == "consistency" {
				job.Work = append(job.Work, jevJobWork{Kind: "consistency", Passages: []string{left.ID}})
			}
			related, err := candidates.Related(ctx, left, 4, false)
			if err != nil {
				return job, err
			}
			for _, right := range related {
				refs := []string{left.ID, right.ID}
				sorted := append([]string(nil), refs...)
				sort.Strings(sorted)
				key := strings.Join(sorted, "\x00")
				if seen[key] {
					continue
				}
				seen[key] = true
				job.Work = append(job.Work, jevJobWork{Kind: input.Kind, Passages: refs})
			}
		}
		if input.Kind == "organization" {
			tags := append([]string(nil), input.Tags...)
			if len(tags) == 0 {
				for _, file := range snapshot.Files {
					tags = append(tags, file.Metadata.Tags...)
				}
			}
			unique := map[string]bool{}
			vocabulary := []string{}
			for _, tag := range tags {
				tag = strings.TrimSpace(tag)
				if tag == "" || len(tag) > 80 || strings.ContainsAny(tag, "\x00\r\n") {
					return job, decision.ErrInvalid
				}
				if !unique[strings.ToLower(tag)] {
					unique[strings.ToLower(tag)] = true
					vocabulary = append(vocabulary, tag)
				}
			}
			sort.Strings(vocabulary)
			for _, passage := range passages {
				if !selected(passage.Path) {
					continue
				}
				for start := 0; start < len(vocabulary); start += 12 {
					job.Work = append(job.Work, jevJobWork{Kind: "tags", Passages: []string{passage.ID}, Tags: append([]string(nil), vocabulary[start:min(start+12, len(vocabulary))]...)})
				}
			}
		}
	}
	return job, nil
}

func validateJevSources(ctx context.Context, store *workspace.Store, job jevJob) error {
	snapshot, err := store.SnapshotScoped(ctx, "", jevActiveScope())
	if err != nil {
		return err
	}
	if jevInventory(snapshot) != job.Inventory {
		return workspace.ErrConflict
	}
	current := make(map[string]workspace.SnapshotFile, len(snapshot.Files))
	for _, file := range snapshot.Files {
		current[file.Path] = file
	}
	for _, note := range job.Notes {
		file, ok := current[note.Path]
		content := []byte(file.Content)
		if !ok {
			// Explicit archived audit sources are outside the active inventory.
			// Read them at the same commit so validation uses one consistent view.
			doc, err := store.KnowledgeRead(ctx, note.Path, snapshot.Revision)
			if err != nil {
				return err
			}
			content = doc.Content
		}
		if workspace.ContentHash(content) != note.CurrentHash {
			return workspace.ErrConflict
		}
		metadata, err := workspace.ParseMetadata(content)
		if err != nil || metadata.ContainerError != "" || metadata.Container == "draft" || metadata.Container != "" && (!note.Source || job.Kind != "audit" || metadata.Container != "archive") {
			return workspace.ErrForbidden
		}
	}
	return nil
}

func loadJevPassages(ctx context.Context, store *workspace.Store, job jevJob) (map[string]knowledge.Passage, error) {
	result := map[string]knowledge.Passage{}
	for _, note := range job.Notes {
		doc, err := store.KnowledgeRead(ctx, note.Path, note.Revision)
		if err != nil {
			return nil, err
		}
		if workspace.ContentHash(doc.Content) != note.Hash {
			return nil, workspace.ErrConflict
		}
		metadata, err := workspace.ParseMetadata(doc.Content)
		if err != nil {
			return nil, err
		}
		passages, err := knowledge.Split(workspace.Snapshot{Revision: note.Revision, Files: []workspace.SnapshotFile{{Path: note.Path, Hash: note.Hash, Content: string(doc.Content), Metadata: metadata}}}, knowledge.Config{PassageRunes: jevPassageRunes})
		if err != nil {
			return nil, err
		}
		for _, passage := range passages {
			if note.Source {
				passage.ID = "source:" + passage.Revision + ":" + passage.ID
			}
			result[passage.ID] = passage
		}
	}
	return result, nil
}

func workEvidence(work jevJobWork, passages map[string]knowledge.Passage) ([]knowledge.JudgmentEvidence, error) {
	evidence := []knowledge.JudgmentEvidence{}
	for _, id := range work.Passages {
		passage, ok := passages[id]
		if !ok {
			return nil, decision.ErrStorage
		}
		evidence = append(evidence, knowledge.JudgmentEvidence{Path: passage.Path, Revision: passage.Revision, Hash: passage.NoteHash, Text: passage.Text, URL: passage.Citation})
	}
	return evidence, nil
}

func evidenceURL(path, revision string) string {
	return (&url.URL{Path: "/" + path, RawQuery: url.Values{"fmn-rev": {revision}}.Encode()}).String()
}

// Bound the serialized Cartesian work graph before allocating it. This leaves
// headroom for verdicts, the job envelope and one current mutation receipt; it
// limits durable metadata size, never token usage or the cost of admitted work.
func preflightJevAudit(ctx context.Context, job jevJob, sources, destinations []knowledge.Passage) error {
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	remaining := int64(4<<20) - int64(len(raw)) - (64 << 10)
	longest := ""
	for _, destination := range destinations {
		if len(destination.ID) > len(longest) {
			longest = destination.ID
		}
	}
	batches := max(1, (len(destinations)+1)/2)
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		refs := []string{source.ID}
		for i := 0; i < min(2, len(destinations)); i++ {
			refs = append(refs, longest)
		}
		encoded, err := json.Marshal(jevJobWork{Kind: "audit", Passages: refs})
		if err != nil {
			return err
		}
		bytesPerWork := int64(len(encoded) + 64)
		if remaining < 0 || int64(batches) > remaining/bytesPerWork {
			return fmt.Errorf("%w: audit metadata needs a smaller source or destination selection", errJevAuditCapacity)
		}
		remaining -= int64(batches) * bytesPerWork
	}
	return nil
}
