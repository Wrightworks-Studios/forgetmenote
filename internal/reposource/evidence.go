package reposource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/durable"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Entry struct {
	BlobID    string `json:"blobId"`
	Path      string `json:"path"`
	Mode      string `json:"mode"`
	Bytes     int64  `json:"bytes"`
	Supported bool   `json:"supported"`
}
type File struct {
	Project   string `json:"project,omitempty"`
	Source    string `json:"source,omitempty"`
	StartLine int    `json:"startLine,omitempty"`
	EndLine   int    `json:"endLine,omitempty"`
	StartByte int    `json:"startByte,omitempty"`
	EndByte   int    `json:"endByte,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	More      bool   `json:"more,omitempty"`
	Path      string `json:"path"`
	Revision  string `json:"revision"`
	Content   string `json:"content"`
	Hash      string `json:"hash"`
	URL       string `json:"url"`
	Retained  bool   `json:"retained"`
	Lines     int    `json:"lines"`
}
type Evidence struct {
	Repository int64  `json:"repository"`
	Name       string `json:"name"`
	Revision   string `json:"revision"`
	Path       string `json:"path"`
	Hash       string `json:"hash"`
	Lines      int    `json:"lines"`
}

func (m *Manager) Image(ctx context.Context, project, id, sha, p string) (out []byte, media string, err error) {
	err = m.read(ctx, project, id, func(c *cache, s State) error {
		if sha == "" {
			return ErrDenied
		}
		var e error
		out, e = c.blob(ctx, sha, p)
		if e != nil {
			return e
		}
		media = http.DetectContentType(out)
		switch media {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			return nil
		default:
			out = nil
			return ErrDenied
		}
	})
	return
}

func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func evidenceID(repo int64, sha, p string) string {
	return hash([]byte(fmt.Sprintf("%d\n%s\n%s", repo, sha, p)))
}
func permalink(repo Repository, sha, p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "https://github.com/" + repo.FullName + "/blob/" + sha + "/" + strings.Join(parts, "/")
}

// Historical names come from retained provenance, never a GitHub redirect or
// an arbitrary repository name supplied in the note's URL.
func (c *cache) citation(ctx context.Context, repo Repository, address string) (sha, p string) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" {
		return "", ""
	}
	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 5)
	if len(parts) != 5 || parts[2] != "blob" || !revision.MatchString(parts[3]) || !safePath(parts[4]) {
		return "", ""
	}
	name := parts[0] + "/" + parts[1]
	if strings.EqualFold(name, repo.FullName) {
		return parts[3], parts[4]
	}
	b, err := c.readEvidence(ctx, "refs/"+evidenceID(repo.ID, parts[3], parts[4])+".json", 8192)
	var e Evidence
	if err != nil || json.Unmarshal(b, &e) != nil || e.Repository != repo.ID || e.Revision != parts[3] || e.Path != parts[4] || !strings.EqualFold(e.Name, name) || !validHash(e.Hash) {
		return "", ""
	}
	return e.Revision, e.Path
}
func (c *cache) tree(ctx context.Context, sha string) ([]Entry, error) {
	if !revision.MatchString(sha) {
		return nil, ErrDenied
	}
	mirror, err := c.mirror(ctx)
	if err != nil {
		return nil, err
	}
	b, err := c.git(ctx, mirror, "", c.limits.Files*1400, "ls-tree", "-r", "-l", "-z", sha)
	if err != nil {
		return nil, ErrUnavailable
	}
	entries := []Entry{}
	for _, line := range strings.Split(string(b), "\x00") {
		if line == "" {
			continue
		}
		meta, p, ok := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 {
			return nil, ErrUnavailable
		}
		if len(entries) >= c.limits.Files {
			return nil, ErrLimit
		}
		n, _ := strconv.ParseInt(fields[3], 10, 64)
		supported := fields[1] == "blob" && (fields[0] == "100644" || fields[0] == "100755") && safePath(p) && n <= c.limits.FileBytes
		entries = append(entries, Entry{Path: p, Mode: fields[0], BlobID: fields[2], Bytes: n, Supported: supported})
	}
	return entries, nil
}
func (c *cache) file(ctx context.Context, repo Repository, sha, p string) (File, error) {
	var f File
	if !validRepo(repo) || !revision.MatchString(sha) || !safePath(p) {
		return f, ErrDenied
	}
	if b, err := c.readEvidence(ctx, "refs/"+evidenceID(repo.ID, sha, p)+".json", 8192); err == nil {
		var e Evidence
		if json.Unmarshal(b, &e) != nil || e.Repository != repo.ID || e.Revision != sha || e.Path != p || !validHash(e.Hash) || !repoName.MatchString(e.Name) {
			return f, ErrUnavailable
		}
		b, err = c.readEvidence(ctx, "blobs/"+e.Hash, c.limits.FileBytes)
		if errors.Is(err, errBlobs) {
			return f, err
		}
		if err != nil || hash(b) != e.Hash {
			return f, ErrUnavailable
		}
		repo.FullName = e.Name
		return fileResult(repo, sha, p, b, true)
	} else if !errors.Is(err, os.ErrNotExist) {
		return f, err
	}
	b, err := c.blob(ctx, sha, p)
	if err != nil {
		return f, err
	}
	return fileResult(repo, sha, p, b, false)
}
func (c *cache) blob(ctx context.Context, sha, p string) ([]byte, error) {
	if !revision.MatchString(sha) || !safePath(p) {
		return nil, ErrDenied
	}
	if b, err := c.pinnedNotebookBlob(ctx, sha, p); err == nil {
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	mirror, err := c.mirror(ctx)
	if err != nil {
		return nil, err
	}
	// A literal pathspec prevents patterns, symlinks and submodule traversal.
	b, err := c.git(ctx, mirror, "", 8192, "--literal-pathspecs", "ls-tree", "-l", "-z", sha, "--", p)
	if err != nil {
		return nil, ErrUnavailable
	}
	meta, name, ok := strings.Cut(strings.TrimSuffix(string(b), "\x00"), "\t")
	fields := strings.Fields(meta)
	if !ok || name != p || len(fields) != 4 {
		return nil, ErrUnavailable
	}
	if fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
		return nil, ErrDenied
	}
	n, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || n < 0 || n > c.limits.FileBytes {
		return nil, ErrLimit
	}
	return c.git(ctx, mirror, "", int(c.limits.FileBytes), "cat-file", "blob", fields[2])
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}
func fileResult(repo Repository, sha, p string, b []byte, retained bool) (File, error) {
	if !utf8.Valid(b) || strings.ContainsAny(string(b), "\x00") || strings.HasPrefix(string(b), "version https://git-lfs.github.com/spec/v1\n") {
		return File{}, ErrDenied
	}
	lines := strings.Count(string(b), "\n")
	if len(b) > 0 && b[len(b)-1] != '\n' {
		lines++
	}
	return File{Path: p, Revision: sha, Content: string(b), Hash: hash(b), URL: permalink(repo, sha, p), Retained: retained, Lines: lines}, nil
}

// retain keeps f as evidence, its content before the reference that names
// it, within the evidence limit given the bytes already stored. It returns
// the bytes it stored, even when it then fails.
func (c *cache) retain(ctx context.Context, repo Repository, f File, stored int64) (int64, error) {
	if !validRepo(repo) || !revision.MatchString(f.Revision) || !safePath(f.Path) || hash([]byte(f.Content)) != f.Hash {
		return 0, ErrDenied
	}
	ref := "refs/" + evidenceID(repo.ID, f.Revision, f.Path) + ".json"
	content := "blobs/" + f.Hash
	e := Evidence{repo.ID, repo.FullName, f.Revision, f.Path, f.Hash, f.Lines}
	b, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	writeRef := true
	existing, err := c.readEvidence(ctx, ref, 8192)
	if err == nil {
		var original Evidence
		if json.Unmarshal(existing, &original) != nil || original.Repository != e.Repository || original.Revision != e.Revision || original.Path != e.Path || original.Hash != e.Hash || original.Lines != e.Lines {
			return 0, ErrConflict
		}
		writeRef = false
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	writeContent := true
	data, err := c.readEvidence(ctx, content, c.limits.FileBytes)
	if err == nil {
		if hash(data) != f.Hash {
			return 0, ErrConflict
		}
		writeContent = false
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	var need int64
	if writeRef {
		need += int64(len(b))
	}
	if writeContent {
		need += int64(len(f.Content))
	}
	if c.blobs == nil {
		// This directory holds every retained file, so it counts them.
		if stored, err = diskBytes(filepath.Join(c.dir, "evidence")); err != nil {
			return 0, err
		}
	}
	if stored+need > c.limits.EvidenceBytes {
		return 0, ErrLimit
	}
	var added int64
	if writeContent {
		if err = c.keepEvidence(ctx, content, []byte(f.Content)); err != nil {
			return added, err
		}
		added += int64(len(f.Content))
	}
	if writeRef {
		if err = c.keepEvidence(ctx, ref, b); err != nil {
			return added, err
		}
		added += int64(len(b))
	}
	return added, nil
}
func atomicFile(name string, b []byte) error {
	dir := filepath.Dir(name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return durable.WriteFile(name, ".write-", b, durable.SyncDir)
}
