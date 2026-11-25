package knowledge

import (
	"encoding/json"
	"forgetmenote/internal/workspace"
	"sort"
)

// ReviewFingerprints bind analysis to its linked sources and backlinks, including
// changed archived copies. They are navigation cache keys, never evidence.
func ReviewFingerprints(snapshot workspace.Snapshot) map[string]string {
	files := map[string]workspace.SnapshotFile{}
	links := map[string][]Reference{}
	incoming := map[string][]string{}
	archived := map[string][]string{}
	pages := snapshotPages(snapshot.Files)
	for _, f := range snapshot.Files {
		files[f.Path] = f
		links[f.Path] = noteLinks(f, pages)
		if s := f.Metadata.Source; s != nil {
			for _, u := range []string{s.URL, s.FinalURL} {
				if u != "" {
					archived[sourceURLKey(u)] = append(archived[sourceURLKey(u)], f.Path+":"+f.Hash)
				}
			}
		}
	}
	for _, f := range snapshot.Files {
		for _, r := range links[f.Path] {
			if r.Path != "" {
				incoming[r.Path] = append(incoming[r.Path], f.Path+":"+f.Hash)
			}
		}
	}
	out := map[string]string{}
	for _, f := range snapshot.Files {
		tokens := append([]string{"visibility:" + snapshot.Visibility}, incoming[f.Path]...)
		for _, r := range links[f.Path] {
			b, _ := json.Marshal(r)
			tokens = append(tokens, string(b))
			if r.Path != "" {
				tokens = append(tokens, r.Path+":"+files[r.Path].Hash)
			}
			if r.External {
				tokens = append(tokens, archived[sourceURLKey(r.URL)]...)
			}
		}
		sort.Strings(tokens)
		b, _ := json.Marshal(tokens)
		out[f.Path] = workspace.ContentHash(b)
	}
	return out
}
