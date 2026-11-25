package knowledge

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"forgetmenote/internal/workspace"
	"net/url"
	"strconv"
)

// DisplayExcerpts derives display ranges from verified pinned content. Small
// notes replay citation segmentation; large notes require a unique exact source
// occurrence, without claiming to revalidate their segmentation IDs. Ambiguous
// text remains unavailable. Passage IDs, hashes and citations stay unchanged.
func DisplayExcerpts(content []byte, passages []Passage, query string) ([]string, error) {
	if len(passages) == 0 {
		return []string{}, nil
	}
	noteHash := workspace.ContentHash(content)
	for _, p := range passages {
		if p.NoteHash != noteHash || workspace.ContentHash([]byte(p.Text)) != p.Hash {
			return nil, errors.New("display source differs from cited passage")
		}
	}
	if len(content) > 64<<10 {
		ranges := make([][2]int, len(passages))
		available := make([]bool, len(passages))
		for i, p := range passages {
			if p.Text == "" {
				continue
			}
			text := []byte(p.Text)
			start := bytes.Index(content, text)
			if start >= 0 && start == bytes.LastIndex(content, text) {
				ranges[i] = [2]int{start, start + len(text)}
				available[i] = true
			}
		}
		out := workspace.MarkdownRangeExcerpts(content, ranges, query)
		for i, ok := range available {
			if !ok {
				out[i] = workspace.UnavailableExcerpt
			}
		}
		return out, nil
	}
	groups := map[int][]int{}
	for i, p := range passages {
		u, err := url.Parse(p.Citation)
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(u.Query().Get("fmn-chars"))
		if err != nil {
			return nil, err
		}
		if p.NoteHash != noteHash {
			return nil, errors.New("display source differs from cited note")
		}
		groups[size] = append(groups[size], i)
	}
	var blobs []string
	ranges := make([][2]int, len(passages))
	found := make([]bool, len(passages))
	for size, indices := range groups {
		p := passages[indices[0]]
		// Current IDs include the real path. History deduplicates splits under
		// history.md and may wrap that ID with the occurrence's path/blob identity.
		for _, path := range []string{p.Path, "history.md"} {
			snapshot := workspace.Snapshot{Revision: p.Revision, Files: []workspace.SnapshotFile{{Path: path, Content: string(content), Hash: p.NoteHash}}}
			_, err := splitPassages(snapshot, Config{PassageRunes: size}, func(candidate Passage, bounds [2]int) {
				for _, i := range indices {
					target := passages[i]
					matches := candidate.ID == target.ID
					if path == "history.md" {
						if blobs == nil {
							blobs = displayGitBlobs(content)
						}
						for _, blob := range blobs {
							version := workspace.ContentHash([]byte(target.Path + "\x00" + blob))
							matches = matches || workspace.ContentHash([]byte(version+"\x00"+candidate.ID)) == target.ID
						}
					}
					if !found[i] && matches && candidate.Hash == target.Hash && candidate.Text == target.Text {
						ranges[i] = bounds
						found[i] = true
					}
				}
			})
			if err != nil {
				return nil, err
			}
			complete := true
			for _, i := range indices {
				complete = complete && found[i]
			}
			if complete {
				break
			}
		}

	}
	for _, ok := range found {
		if !ok {
			return nil, errors.New("display passage differs from cited note")
		}
	}
	return workspace.MarkdownRangeExcerpts(content, ranges, query), nil
}

// Git's supported object formats hash the same canonical blob framing. This
// recovers historical occurrence IDs without launching Git or altering them.
func displayGitBlobs(content []byte) []string {
	header := []byte("blob " + strconv.Itoa(len(content)) + "\x00")
	one := sha1.New()
	one.Write(header)
	one.Write(content)
	two := sha256.New()
	two.Write(header)
	two.Write(content)
	return []string{hex.EncodeToString(one.Sum(nil)), hex.EncodeToString(two.Sum(nil))}
}
