package knowledge

import (
	"forgetmenote/internal/workspace"
	"unsafe"
)

// EstimatedBytes is a conservative retention budget, not a heap measurement.
// Text shared by projections may be counted more than once. Active requests are
// protected independently; this estimate controls only idle cache retention.
func (e *Engine) EstimatedBytes() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	size := int64(32<<10) + int64(e.writingCacheBytes)
	for key, analysis := range e.writingCache {
		size += int64(256+len(key)) + int64(cap(analysis.Sections))*int64(unsafe.Sizeof(WritingSection{})) + int64(cap(analysis.blocks))*int64(unsafe.Sizeof(writingBlock{}))
		for _, section := range analysis.Sections {
			size += int64(len(section.Title) + len(section.Kind))
		}
		for _, block := range analysis.blocks {
			size += int64(len(block.text)) + int64(cap(block.starts)+cap(block.ends))*int64(unsafe.Sizeof(int(0)))
		}
	}
	for _, file := range e.snapshot.Files {
		size += int64(512+2*len(file.Content)+len(file.Path)) + int64(cap(file.Metadata.Tags))*int64(unsafe.Sizeof(""))
	}
	for _, passage := range e.passages {
		size += passageRetentionBytes(passage)
	}
	for hash, vector := range e.cache.Vectors {
		size += int64(256 + len(hash) + cap(vector)*4)
	}
	for _, blob := range e.history.Blobs {
		size += int64(512+2*len(blob.Content)+len(blob.Path)) + int64(cap(blob.Metadata.Tags))*int64(unsafe.Sizeof(""))
	}
	for _, version := range e.history.Versions {
		size += int64(256 + len(version.Path) + len(version.Blob) + cap(version.Revisions)*int(unsafe.Sizeof(workspace.HistoryRevision{})))
		for _, revision := range version.Revisions {
			size += int64(len(revision.Revision) + len(revision.Time))
		}
	}
	size += int64(cap(e.history.Queue)) * int64(unsafe.Sizeof(workspace.HistoryRevision{}))
	for _, revision := range e.history.Queue {
		size += int64(len(revision.Revision) + len(revision.Time))
	}
	for _, passages := range e.historySplits {
		for _, passage := range passages {
			size += passageRetentionBytes(passage)
		}
	}
	if e.retrieval != nil {
		size += e.retrieval.terms.EstimatedBytes()
	}
	if e.mapCache != nil {
		for path, entry := range e.mapCache.Entries {
			size += int64(512 + len(path) + len(entry.Title) + len(entry.Hash))
			for _, heading := range entry.Headings {
				size += int64(64 + len(heading.Text) + len(heading.Anchor))
			}
			for _, values := range [][]string{entry.Tags, entry.Links, entry.Backlinks, entry.PageNames, entry.PageLinks} {
				for _, value := range values {
					size += int64(32 + len(value))
				}
			}
		}
	}
	for _, references := range e.referenceLinks {
		for _, reference := range references {
			size += int64(512 + len(reference.URL) + len(reference.Label) + len(reference.Note) + len(reference.Title) + len(reference.Path) + len(reference.Project) + len(reference.SourceProject) + len(reference.Heading) + len(reference.Notebook))
		}
	}
	for key, checkpoint := range e.state.Checkpoints {
		size += int64(512 + len(key) + len(checkpoint.Summary))
	}
	for key, proposal := range e.state.Proposals {
		size += int64(2048 + len(key) + len(proposal.Group.Summary))
		for path, content := range proposal.Before {
			size += int64(64 + len(path) + len(content))
		}
		for _, edit := range proposal.Group.Edits {
			size += int64(512 + len(edit.Path) + len(edit.Content))
		}
	}
	return size
}
func (e *Engine) BackgroundRequired() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.Config.Enabled && e.state.Config.Endpoint && e.provider.Load() != nil || e.historyLoaded || e.historyResume || e.state.Config.Enabled && e.currentVectorPersistFailure != ""
}

func passageRetentionBytes(p Passage) int64 {
	return int64(512 + len(p.Text) + len(p.Path) + len(p.Heading) + len(p.Citation) + len(p.Anchor) + len(p.NoteID) + len(p.Container) + len(p.ID) + len(p.Hash) + len(p.NoteHash) + len(p.SectionHash) + len(p.Revision))
}
