package reposource

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/procgroup"
)

type LineRange struct {
	StartLine int    `json:"startLine,omitempty"`
	EndLine   int    `json:"endLine,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
}

func (r LineRange) Validate() error {
	if r.StartLine < 1 || r.EndLine < r.StartLine || r.EndLine-r.StartLine >= 200 || len(r.Cursor) > 4096 {
		return ErrDenied
	}
	return nil
}

type rangeCursor struct {
	Binding string `json:"binding"`
	Offset  int    `json:"offset"`
}

func (m *Manager) FileRange(ctx context.Context, project, id, sha, p string, r LineRange) (File, error) {
	if err := r.Validate(); err != nil {
		return File{}, err
	}
	var out File
	err := m.read(ctx, project, id, func(c *cache, s State) error {
		if sha == "" {
			sha = s.Downloaded
		}
		var err error
		out, err = c.fileRange(ctx, s, sha, p, r)
		out.Project = s.Project
		out.Source = s.ID
		return err
	})
	return out, err
}
func (c *cache) fileRange(ctx context.Context, s State, sha, p string, r LineRange) (File, error) {
	if r.Validate() != nil || !safePath(p) || !revision.MatchString(sha) {
		return File{}, ErrDenied
	}
	binding := hash([]byte(fmt.Sprintf("%s\n%s\n%s\n%d\n%d", s.ID, sha, p, r.StartLine, r.EndLine)))
	resume := 0
	if r.Cursor != "" {
		var cursor rangeCursor
		b, err := base64.RawURLEncoding.DecodeString(r.Cursor)
		if err != nil || json.Unmarshal(b, &cursor) != nil || cursor.Binding != binding || cursor.Offset < 0 {
			return File{}, ErrDenied
		}
		resume = cursor.Offset
	}
	mirror, err := c.mirror(ctx)
	if err != nil {
		return File{}, err
	}
	b, err := c.git(ctx, mirror, "", 8192, "--literal-pathspecs", "ls-tree", "-l", "-z", sha, "--", p)
	if err != nil {
		return File{}, ErrUnavailable
	}
	meta, name, ok := strings.Cut(strings.TrimSuffix(string(b), "\x00"), "\t")
	fields := strings.Fields(meta)
	if !ok || name != p || len(fields) != 4 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
		return File{}, ErrDenied
	}
	size, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || size < 0 || int64(resume) > size {
		return File{}, ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.limits.Seconds)*time.Second)
	defer cancel()
	cmd := c.command(ctx, mirror, "", "cat-file", "blob", fields[2])
	cmd.Stderr = &procgroup.Output{Limit: 65536, Err: ErrLimit}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return File{}, err
	}
	if err = cmd.Start(); err != nil {
		return File{}, ErrUnavailable
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	reader := bufio.NewReaderSize(pipe, 65536)
	prefix, _ := reader.Peek(min(512, int(size)))
	if strings.IndexByte(string(prefix), 0) >= 0 || strings.HasPrefix(string(prefix), "version https://git-lfs.github.com/spec/v1") {
		return File{}, ErrDenied
	}
	out := File{Path: p, Revision: sha}
	var text strings.Builder
	line, offset := 1, 0
	last := 0
	for {
		if err = ctx.Err(); err != nil {
			return File{}, err
		}
		if line > r.EndLine {
			break
		}
		v, n, e := reader.ReadRune()
		if e == io.EOF {
			break
		}
		if e != nil {
			return File{}, ErrUnavailable
		}
		if v == utf8.RuneError && n == 1 || v == 0 {
			return File{}, ErrDenied
		}
		if offset < resume && offset+n > resume {
			return File{}, ErrDenied
		}
		if line >= r.StartLine && offset >= resume {
			if out.StartLine == 0 {
				out.StartLine = line
				out.StartByte = offset
			}
			if text.Len()+n > 65536 {
				data, _ := json.Marshal(rangeCursor{binding, offset})
				out.Cursor = base64.RawURLEncoding.EncodeToString(data)
				break
			}
			text.WriteRune(v)
			out.EndByte = offset + n
			last = line
		}
		offset += n
		if v == '\n' {
			line++
		}
	}
	if resume > 0 && (out.StartByte != resume || out.StartLine < r.StartLine) {
		return File{}, ErrDenied
	}
	out.EndLine = last
	out.Content = text.String()
	out.More = out.Content != "" && int64(out.EndByte) < size
	out.Hash = hash([]byte(out.Content))
	if out.StartLine > 0 {
		out.Lines = last - out.StartLine + 1
		out.URL = permalink(s.Repository, sha, p) + fmt.Sprintf("#L%d-L%d", out.StartLine, last)
	} else {
		out.URL = permalink(s.Repository, sha, p)
	}
	return out, nil
}
