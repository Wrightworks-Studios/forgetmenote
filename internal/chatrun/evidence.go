package chatrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"forgetmenote/internal/actions"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/dataops"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/notebooks"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/search"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

// Each citation retains the dependencies from the same committed snapshot as its
// text. Later retrieval cannot renew the provenance of an earlier answer.
func PinCitationReferences(ctx context.Context, store *workspace.Store, citations []chat.Citation) error {
	snapshots := map[string]workspace.Snapshot{}
	fingerprints := map[string]map[string]string{}
	current, err := ScopedSnapshot(ctx, store, "")
	if err != nil {
		return err
	}
	for i := range citations {
		citation := &citations[i]
		if citation.Scope == "attachment" || citation.Kind == "repository" || citation.Kind == "notebook" {
			continue
		}
		if citation.Scope == "" {
			citation.Scope = "current"
			if scope := chat.RunScopeFrom(ctx); scope != nil && scope.Run.Options.Corpus.History && citation.Revision != current.Revision {
				citation.Scope = "history"
			}
		}
		if citation.Path == "" || citation.Revision == "" || citation.Hash == "" {
			continue
		}
		snapshot, exists := snapshots[citation.Revision]
		if !exists {
			// The current snapshot already holds this corpus at its revision.
			snapshot = current
			if citation.Revision != current.Revision {
				if snapshot, err = ScopedSnapshot(ctx, store, citation.Revision); err != nil {
					return err
				}
			}
			snapshots[citation.Revision] = snapshot
			fingerprints[citation.Revision] = knowledge.ReviewFingerprints(snapshot)
		}
		for _, file := range snapshot.Files {
			if file.Path == citation.Path && file.Hash == citation.Hash {
				citation.References = fingerprints[citation.Revision][citation.Path]
				break
			}
		}
	}
	return nil
}

// RetrievalCitations cites retrieved passages, source excerpts and other
// notebooks' passages. A tool call's citation IDs carry its operation ID; the
// initial retrieval's do not.
func RetrievalCitations(project, operation string, found chatRetrieval) []chat.Citation {
	id := func(kind string, i int) string {
		if operation != "" {
			return fmt.Sprintf("%s-%s-%d", kind, operation, i+1)
		}
		return fmt.Sprintf("%s-%d", kind, i+1)
	}
	citations := []chat.Citation{}
	for i, p := range found.Passages {
		citations = append(citations, passageCitation(id("source", i), project, p))
	}
	for i, p := range found.Sources {
		citations = append(citations, sourceCitation(id("repo", i), p))
	}
	for i, p := range found.Notes {
		citations = append(citations, notebookCitation(id("notebook", i), p))
	}
	return citations
}

// RecordToolResult adds a finished call's new citations to its run and,
// when the run may continue from it, pairs the call with its model-visible
// result. A revoked run keeps the citations, which still gate access to the
// transcript, but the result never enters its continuation.
func RecordToolResult(c *chat.Conversation, r *chat.Run, toolCallID, content string, citations []chat.Citation, continues bool) {
	if chat.AddCitations(r, citations) {
		c.AddEvent(r.ID, "citations", r.Citations)
	}
	if continues {
		r.Messages = append(r.Messages, chat.Message{Role: "tool", ToolCallID: toolCallID, Content: content})
	}
}

// A read acknowledgement supplies the exact original bytes and revision. Keep
// its citation even if a later permission/owner failure prevents dependency
// pinning; the incomplete provenance then prevents reuse in future context.
func (s *Service) ToolEvidence(ctx context.Context, auth chat.RunAuth, operationID, name string, result json.RawMessage) ([]chat.Citation, bool) {
	if name == "call_data_operation" {
		var v dataops.Result
		if json.Unmarshal(result, &v) != nil || v.Receipt.ID == "" {
			return nil, false
		}
		return []chat.Citation{{ID: v.Receipt.ID, Project: auth.Project, Source: v.Receipt.Connection, References: v.Receipt.Operation, URL: "#data-receipt-" + v.Receipt.ID, Title: v.Receipt.Connection + " · " + v.Receipt.Operation, Text: TextLimit(string(result), 12000), CapturedAt: v.Receipt.Time, Hash: v.Receipt.OutputHash, Kind: "data"}}, false
	}
	if name == "read_document" || name == "search_notebooks" {
		var citations []chat.Citation
		if name == "read_document" {
			var doc notebooks.DocumentOutput
			if json.Unmarshal(result, &doc) != nil || doc.Project == "" || doc.Path == "" || doc.Revision == "" || doc.Hash == "" || doc.Citation == "" {
				return nil, false
			}
			if doc.Origin != "notes" && doc.Origin != "repository" || doc.Origin == "repository" && doc.Source == "" {
				return nil, false
			}
			citation := chat.Citation{ID: "document-" + operationID, Project: doc.Project, Source: doc.Source, Path: doc.Path, Revision: doc.Revision, URL: doc.Citation, Text: TextLimit(doc.Content, 12000), Hash: doc.Hash, Kind: "notebook", Scope: "notebook"}
			if doc.Origin == "repository" {
				citation.Kind, citation.Scope = "repository", "repository"
				citation = chatSourceExcerpt(citation, doc.Content, doc.StartByte, max(1, doc.StartLine))
			}
			citations = append(citations, citation)
		} else {
			var found search.Result
			if json.Unmarshal(result, &found) != nil {
				return nil, false
			}
			for i, item := range found.Results {
				id := fmt.Sprintf("document-%s-%d", operationID, i)
				if item.Source != nil {
					citations = append(citations, sourceCitation(id, *item.Source))
				} else if item.Note != nil {
					citations = append(citations, notebookCitation(id, notebookEvidence{Project: item.Project, Passage: *item.Note}))
				}
			}
		}
		return citations, false // Revalidate by project; never treat another notebook as a local dependency.
	}

	if name == "search_repository_sources" || name == "repository_source_file" {
		var citations []chat.Citation
		if name == "search_repository_sources" {
			var found reposource.SearchResult
			if json.Unmarshal(result, &found) != nil {
				return nil, false
			}
			for i, p := range found.Results {
				citations = append(citations, sourceCitation(fmt.Sprintf("repo-%s-%d", operationID, i), p))
			}
		} else {
			var f reposource.File
			if json.Unmarshal(result, &f) != nil {
				return nil, false
			}
			start := f.StartLine
			if start == 0 {
				start = 1
			}
			citations = []chat.Citation{chatSourceExcerpt(chat.Citation{ID: "repo-" + operationID, Project: f.Project, Source: f.Source, Path: f.Path, URL: f.URL, Revision: f.Revision, Kind: "repository", Scope: "repository"}, f.Content, f.StartByte, start)}
		}
		for _, c := range citations {
			if _, err := s.catalog.Access(auth.User, c.Project, "read"); err != nil {
				return nil, false
			}
			if err := s.hooks.CheckSourceScope(ctx, c.Project); err != nil {
				return nil, false
			}
			state, err := s.hooks.Repositories().Status(c.Project)
			if err != nil || state.Downloaded != c.Revision || state.ID != c.Source {
				return nil, false
			}
		}
		return citations, false // Source evidence is revalidated separately, never as a note dependency.
	}
	if name != "read_note" {
		return nil, false
	}
	var note webcore.NoteOutput
	if json.Unmarshal(result, &note) != nil || note.Path == "" || note.Revision == "" {
		return nil, false
	}
	link := url.URL{Path: "/" + note.Path, RawQuery: url.Values{"fmn-rev": {note.Revision}}.Encode()}
	citations := []chat.Citation{{ID: "read-" + operationID, Project: auth.Project, Path: note.Path, URL: link.String(), Title: note.Path, Text: TextLimit(note.Content, 12000), Revision: note.Revision, Hash: workspace.ContentHash([]byte(note.Content)), Kind: "committed"}}
	if s.hooks.CheckRun(auth) != nil {
		return citations, false
	}
	store, err := s.catalog.Access(auth.User, auth.Project, "read")
	if err != nil {
		return citations, false
	}
	if err = PinCitationReferences(ctx, store, citations); err != nil {
		return citations, false
	}
	return citations, citations[0].References != ""
}

// chatSourceExcerpt cites at most the first 12000 bytes of a repository
// source's content, ending on a character boundary. The excerpt's hash, byte
// range and line range are those of the bytes actually cited, so they stay
// exact even when the content is longer.
func chatSourceExcerpt(citation chat.Citation, content string, startByte, startLine int) chat.Citation {
	end := min(len(content), 12000)
	for end < len(content) && end > 0 && !utf8.RuneStart(content[end]) {
		end--
	}
	text := content[:end]
	citation.Text, citation.Hash = text, workspace.ContentHash([]byte(text))
	citation.StartByte, citation.EndByte = startByte, startByte+len(text)
	citation.StartLine = startLine
	citation.EndLine = startLine + strings.Count(strings.TrimSuffix(text, "\n"), "\n")
	return citation
}

func ToolResultContext(name string, result json.RawMessage, citations []chat.Citation) string {
	if p := actions.LookupPolicy(name); p.Control || p.Edit || p.Proposal {
		return ControlResult(name, result)
	}
	if compact, ok := CompactSourceResult(name, result, citations); ok {
		return compact
	}
	text := string(result)
	if len(citations) == 0 {
		return text
	}
	metadata := append([]chat.Citation(nil), citations...)
	for i := range metadata {
		metadata[i].Text = ""
	}
	raw, _ := json.Marshal(metadata)
	return text + "\nEvidence references (live results are dated captures, not notebook revisions): " + string(raw)
}

// History dependencies are durable validation metadata, not additional displayed
// citations. Bound the closure; an oversized closure remains local but is not
// reused as supposedly complete evidence in another model request.
func inheritChatProvenance(target *chat.Provenance, turn chat.Turn) {
	if target.CorpusID == "" || target.CorpusID != turn.Provenance.CorpusID || !turn.Provenance.Complete || !turn.Provenance.AttachmentsTracked {
		target.Complete = false
		return
	}
	if turn.Provenance.SnapshotBound || len(turn.Citations) == 0 && len(turn.Provenance.Dependencies) == 0 && len(turn.Provenance.AttachmentReferences) == 0 {
		target.SnapshotBound = true
	}
	inheritExternalEvidence(target, turn.Provenance)
	for _, reference := range turn.Provenance.AttachmentReferences {
		addChatAttachmentReference(target, reference)
	}
	if target.Dependencies == nil {
		target.Dependencies = map[string]string{}
	}
	add := func(path, fingerprint string) {
		if path == "" {
			return
		}
		if fingerprint == "" {
			target.Complete = false
			return
		}
		if prior, ok := target.Dependencies[path]; ok {
			if prior != fingerprint {
				target.Complete = false
			}
			return
		}
		bytes := len(path) + len(fingerprint)
		for p, v := range target.Dependencies {
			bytes += len(p) + len(v)
		}
		if len(target.Dependencies) >= 256 || bytes > 64<<10 {
			target.Complete = false
			return
		}
		target.Dependencies[path] = fingerprint
	}
	for _, dependency := range turn.Provenance.HistoricalDependencies {
		addHistoricalDependency(target, dependency)
	}
	for path, fingerprint := range turn.Provenance.Dependencies {
		add(path, fingerprint)
	}
	for _, citation := range turn.Citations {
		if citation.Scope == "attachment" {
			continue
		}
		if citation.Scope == "history" {
			addHistoricalDependency(target, chat.HistoricalDependency{Path: citation.Path, Revision: citation.Revision, Hash: citation.Hash, References: citation.References})
			continue
		}
		if citation.Path == "" {
			continue
		}
		if citation.Hash == "" || citation.References == "" {
			target.Complete = false
			continue
		}
		add(citation.Path, citation.Hash+":"+citation.References)
	}
}

func cloneChatProvenance(source chat.Provenance) chat.Provenance {
	copy := source
	copy.HistoricalDependencies = append([]chat.HistoricalDependency(nil), source.HistoricalDependencies...)
	copy.Dependencies = map[string]string{}
	for path, fingerprint := range source.Dependencies {
		copy.Dependencies[path] = fingerprint
	}
	copy.ExternalSources = append([]chat.ExternalCapture(nil), source.ExternalSources...)
	copy.AttachmentReferences = append([]chat.AttachmentReference(nil), source.AttachmentReferences...)
	return copy
}

func attachmentReference(turnID string, attachment chat.Attachment) chat.AttachmentReference {
	return chat.AttachmentReference{TurnID: turnID, Kind: attachment.Kind, Path: attachment.Path, Revision: attachment.Revision, LiveRevision: attachment.LiveRevision, Hash: workspace.ContentHash([]byte(attachment.Content))}
}
func attachmentReferenceKey(reference chat.AttachmentReference) string {
	reference.References = ""
	raw, _ := json.Marshal(reference)
	return string(raw)
}
func addChatAttachmentReference(provenance *chat.Provenance, reference chat.AttachmentReference) {
	key := attachmentReferenceKey(reference)
	bytes := len(key) + len(reference.References)
	for _, old := range provenance.AttachmentReferences {
		oldKey := attachmentReferenceKey(old)
		if oldKey == key {
			if old.References != reference.References {
				provenance.Complete = false
			}
			return
		}
		bytes += len(oldKey) + len(old.References)
	}
	if reference.TurnID == "" || len(provenance.AttachmentReferences) >= 64 || bytes > 64<<10 {
		provenance.Complete = false
		return
	}
	provenance.AttachmentReferences = append(provenance.AttachmentReferences, reference)
}

// Explicit noncommitted input stays the exact immutable accepted snapshot. Never
// read a newer shared document or ask the browser for a draft during validation.
func (s *Service) chatAttachmentProvenance(ctx context.Context, store *workspace.Store, turn chat.Turn, provenance *chat.Provenance) ([]chat.Citation, error) {
	provenance.AttachmentsTracked = true
	citations := []chat.Citation{}
	for i, attachment := range turn.Attachments {
		reference := attachmentReference(turn.ID, attachment)
		if attachment.Hash != "" && attachment.Hash != reference.Hash {
			provenance.Complete = false
			continue
		}
		switch attachment.Kind {
		case "committed":
			if attachment.Revision == "" {
				provenance.Complete = false
				continue
			}
			link := url.URL{Path: "/" + attachment.Path, RawQuery: url.Values{"fmn-rev": {attachment.Revision}}.Encode()}
			evidence := []chat.Citation{{ID: fmt.Sprintf("attachment-%d", i+1), Path: attachment.Path, URL: link.String(), Title: attachment.Path, Revision: attachment.Revision, Hash: reference.Hash, Kind: "committed", Scope: "attachment", References: "attachment:" + reference.Hash}}
			if err := PinCitationReferences(ctx, store, evidence); err != nil {
				return nil, err
			}
			reference.References = evidence[0].References
			inheritChatProvenance(provenance, chat.Turn{Provenance: chat.Provenance{CorpusID: provenance.CorpusID, Complete: true, AttachmentsTracked: true}, Citations: evidence})
			citations = append(citations, evidence...)
		case "live":
			if attachment.LiveRevision == "" {
				provenance.Complete = false
			}
		case "private":
		default:
			provenance.Complete = false
			continue
		}
		addChatAttachmentReference(provenance, reference)
	}
	return citations, nil
}

// Construct once per authorized snapshot; validating many cached notes must not
// rebuild the notebook reference graph for every individual analysis.
func chatEvidenceValidator(snapshot workspace.Snapshot, instructionsHash string, turns []chat.Turn, corpusID string, historical ...func(chat.HistoricalDependency) bool) func(chat.Provenance, []chat.Citation) bool {
	fingerprints := knowledge.ReviewFingerprints(snapshot)
	notes := map[string]string{}
	for _, file := range snapshot.Files {
		if !workspace.IsTrash(file.Path) {
			notes[file.Path] = file.Hash + ":" + fingerprints[file.Path]
		}
	}
	attachments := map[string]bool{}
	attachmentSources := map[chat.HistoricalDependency]bool{}
	for _, turn := range turns {
		if turn.Role != "user" || turn.ID == "" {
			continue
		}
		for _, attachment := range turn.Attachments {
			reference := attachmentReference(turn.ID, attachment)
			if attachment.Hash == "" || attachment.Hash == reference.Hash {
				attachments[attachmentReferenceKey(reference)] = true
				if reference.Kind == "committed" {
					attachmentSources[chat.HistoricalDependency{Path: reference.Path, Revision: reference.Revision, Hash: reference.Hash, References: "attachment:" + reference.Hash}] = true
				}
			}
		}
	}
	return func(provenance chat.Provenance, citations []chat.Citation) bool {
		if corpusID == "" || provenance.CorpusID != corpusID || provenance.ExternalAllowance != "" || len(provenance.ExternalSources) > 0 || !provenance.Complete || !provenance.AttachmentsTracked || provenance.InstructionsHash != instructionsHash {
			return false
		}
		if (provenance.SnapshotBound || len(citations) == 0 && len(provenance.Dependencies) == 0 && len(provenance.AttachmentReferences) == 0) && (provenance.Revision != snapshot.Revision || provenance.Visibility != snapshot.Visibility) {
			return false
		}
		if len(provenance.HistoricalDependencies) > 256 {
			return false
		}
		historicalBytes := 0
		for _, dependency := range provenance.HistoricalDependencies {
			historicalBytes += len(dependency.Path) + len(dependency.Revision) + len(dependency.Hash) + len(dependency.References)
			if len(historical) != 1 || !historical[0](dependency) {
				return false
			}
		}
		if historicalBytes > 64<<10 {
			return false
		}
		for path, dependency := range provenance.Dependencies {
			if dependency == "" || notes[path] != dependency {
				return false
			}
		}
		for _, citation := range citations {
			dependency := chat.HistoricalDependency{Path: citation.Path, Revision: citation.Revision, Hash: citation.Hash, References: citation.References}
			if citation.Scope == "attachment" {
				if !attachmentSources[dependency] {
					return false
				}
				continue
			}
			if citation.Scope == "history" {
				if len(historical) != 1 || !historical[0](dependency) {
					return false
				}
				continue
			}
			if citation.Scope != "" && citation.Scope != "current" {
				return false
			}
			if citation.Kind == "external" {
				return false
			}
			if citation.Path != "" && (citation.Hash == "" || citation.References == "" || notes[citation.Path] != citation.Hash+":"+citation.References) {
				return false
			}
		}
		if len(provenance.Dependencies) > 256 || len(provenance.AttachmentReferences) > 64 {
			return false
		}
		dependencyBytes, attachmentBytes := 0, 0
		for path, fingerprint := range provenance.Dependencies {
			dependencyBytes += len(path) + len(fingerprint)
		}
		for _, reference := range provenance.AttachmentReferences {
			attachmentBytes += len(attachmentReferenceKey(reference)) + len(reference.References)
		}
		if dependencyBytes > 64<<10 || attachmentBytes > 64<<10 {
			return false
		}
		for _, reference := range provenance.AttachmentReferences {
			if !attachments[attachmentReferenceKey(reference)] {
				return false
			}
			switch reference.Kind {
			case "committed":
				if reference.Revision == "" || reference.References != "attachment:"+reference.Hash {
					return false
				}
			case "live":
				if reference.LiveRevision == "" {
					return false
				}
			case "private":
			default:
				return false
			}
		}
		return true
	}
}

func addHistoricalDependency(provenance *chat.Provenance, dependency chat.HistoricalDependency) {
	if dependency.Path == "" || dependency.Revision == "" || dependency.Hash == "" || dependency.References == "" {
		provenance.Complete = false
		return
	}
	size := len(dependency.Path) + len(dependency.Revision) + len(dependency.Hash) + len(dependency.References)
	for _, old := range provenance.HistoricalDependencies {
		if old == dependency {
			return
		}
		size += len(old.Path) + len(old.Revision) + len(old.Hash) + len(old.References)
	}
	if len(provenance.HistoricalDependencies) >= 256 || size > 64<<10 {
		provenance.Complete = false
		return
	}
	provenance.HistoricalDependencies = append(provenance.HistoricalDependencies, dependency)
}

func chatHistoricalValidator(ctx context.Context, store *workspace.Store, run chat.Run, current workspace.Snapshot) func(chat.HistoricalDependency) bool {
	cache := map[string]map[string]string{}
	return func(dependency chat.HistoricalDependency) bool {
		if run.Options.Corpus.Validate() != nil || !run.Options.Corpus.History || dependency.Path == "" || dependency.Revision == "" || dependency.Hash == "" || dependency.References == "" {
			return false
		}
		notes, ok := cache[dependency.Revision]
		if !ok {
			notes = map[string]string{}
			snapshot, err := CorpusSnapshot(ctx, store, run, dependency.Revision)
			if err != nil || snapshot.Visibility != current.Visibility {
				return false
			}
			fingerprints := knowledge.ReviewFingerprints(snapshot)
			for _, file := range snapshot.Files {
				notes[file.Path] = file.Hash + ":" + fingerprints[file.Path]
			}
			cache[dependency.Revision] = notes
		}
		return notes[dependency.Path] == dependency.Hash+":"+dependency.References
	}
}
