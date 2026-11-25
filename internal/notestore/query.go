package notestore

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"strings"
	"time"

	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
)

// The trailers that name the operation a commit answers, and its request.
const (
	OperationTrailer = "Gitnotes-Operation"
	RequestTrailer   = "Gitnotes-Request"
)

// CommitByOperation returns the newest revision whose Gitnotes-Operation
// trailer is operation, as "git log -1 --grep" finds the commit a retried
// operation already made. Revisions Apply wrote and imported ones alike are
// found; ok is false when there is none.
func (n *Notebook) CommitByOperation(ctx context.Context, operation string) (info CommitInfo, ok bool, err error) {
	if operation == "" {
		return CommitInfo{}, false, nil
	}
	infos, err := n.commits(ctx, `SELECT seq FROM revisions WHERE org = $1 AND notebook = $2 AND operation_id = $3 ORDER BY seq DESC LIMIT 1`, true, operation)
	if err != nil || len(infos) == 0 {
		return CommitInfo{}, false, err
	}
	return infos[0], true, nil
}

// operationMessage makes message record operation and request as its
// Gitnotes-Operation and Gitnotes-Request trailers. A message that already
// carries both, where its writer always put them, is kept as it is; one that
// carries neither gets them at the end, as "git commit --trailer" adds them.
func operationMessage(message, operation, request string) (string, error) {
	if request == "" {
		return message, nil
	}
	if operation == "" {
		return "", invalid("a request ID needs an operation ID")
	}
	for _, v := range []string{operation, request} {
		if strings.ContainsAny(v, "\r\n") || strings.TrimSpace(v) != v {
			return "", invalid("operation or request ID %q", v)
		}
	}
	var operations, requests []string
	for _, t := range gitobj.Trailers(message) {
		switch {
		case strings.EqualFold(t.Key, OperationTrailer):
			operations = append(operations, t.Value)
		case strings.EqualFold(t.Key, RequestTrailer):
			requests = append(requests, t.Value)
		}
	}
	switch {
	case len(operations) == 0 && len(requests) == 0:
		return gitobj.CleanMessage(message, "", []gitobj.Trailer{{Key: OperationTrailer, Value: operation}, {Key: RequestTrailer, Value: request}}), nil
	case len(operations) == 1 && len(requests) == 1 && operations[0] == operation && requests[0] == request:
		return message, nil
	}
	return "", invalid("the message's operation trailers are not operation %s and request %s", operation, request)
}

// Receipts returns the notebook's receipts of kind, oldest first.
func (n *Notebook) Receipts(ctx context.Context, kind string) ([]Receipt, error) {
	return n.receipts(ctx, kind, "")
}

// PendingReceipts returns the notebook's receipts of kind that are not
// done, oldest first. The done ones, kept for ReceiptTTL, are not read.
func (n *Notebook) PendingReceipts(ctx context.Context, kind string) ([]Receipt, error) {
	return n.receipts(ctx, kind, " AND done IS NULL")
}

func (n *Notebook) receipts(ctx context.Context, kind, filter string) ([]Receipt, error) {
	rows, err := n.s.db.Pool.Query(ctx, `SELECT operation_id, request_hash, state, result, created, done FROM operation_receipts
		WHERE org = $1 AND notebook = $2 AND kind = $3`+filter+` ORDER BY created, operation_id`, n.s.db.Org, n.id, kind)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Receipt, error) {
		r := Receipt{Kind: kind}
		var done *time.Time
		err := row.Scan(&r.Operation, &r.RequestHash, &r.State, &r.Result, &r.Created, &done)
		if done != nil {
			r.Done = *done
		}
		return r, err
	})
}

// historyRange answers a HistoryQuery that names no entry or path: every
// revision of the notebook, or those that changed a file at or under Prefix,
// and of those the ones whose message has a line containing MessageContains.
func (n *Notebook) historyRange(ctx context.Context, q HistoryQuery) ([]CommitInfo, error) {
	if strings.ContainsAny(q.MessageContains, "\n") {
		return nil, invalid("a message search matches within one line")
	}
	before := q.Before
	if before <= 0 {
		before = math.MaxInt64
	}
	var limit any
	if q.Limit > 0 {
		limit = int64(q.Limit)
	}
	query := `SELECT seq FROM revisions r WHERE org = $1 AND notebook = $2 AND seq < $3`
	args := []any{before, limit}
	if q.MessageContains != "" {
		// The message follows the headers' blank line; Git searches only it.
		args = append(args, []byte(q.MessageContains))
		query += fmt.Sprintf(` AND position($%d::bytea IN substring(commit_body FROM position('\x0a0a'::bytea IN commit_body) + 2)) > 0`, len(args)+2)
	}
	if prefix := strings.Trim(q.Prefix, "/"); prefix != "" {
		// Paths compare bytewise, so the paths under prefix/ are those from
		// "prefix/" up to "prefix0", '0' following '/'.
		args = append(args, prefix, prefix+"/", prefix+"0")
		p, low, high := len(args), len(args)+1, len(args)+2
		under := func(column string) string {
			return fmt.Sprintf(`c.%[1]s = $%[2]d OR c.%[1]s >= $%[3]d AND c.%[1]s < $%[4]d`, column, p, low, high)
		}
		query += ` AND EXISTS (SELECT FROM revision_changes c WHERE c.org = r.org AND c.notebook = r.notebook AND c.seq = r.seq AND (` + under("path") + ` OR ` + under("old_path") + `))`
	}
	return n.commits(ctx, query+` ORDER BY seq DESC LIMIT $4`, true, args...)
}

// PathVersion returns the newest revision at or before rev, the zero ID
// meaning the head, that added, changed, renamed or deleted the file at path,
// as "git log -1 rev -- path" names it: a file's Version while it exists, and
// the revision that removed it once it is gone. It is the zero ID when no
// revision up to rev touched path.
func (n *Notebook) PathVersion(ctx context.Context, rev gitobj.ID, path string) (gitobj.ID, error) {
	m, r, err := n.resolve(ctx, rev)
	if err != nil {
		return gitobj.ID{}, err
	}
	if m != nil {
		if f, ok := m.files[path]; ok {
			return f.Version, nil
		}
	}
	pool := n.s.db.Pool
	line, err := n.firstParentLine(ctx, pool, r.seq)
	if err != nil {
		return gitobj.ID{}, err
	}
	newest := func(column string) string {
		query := `SELECT max(seq) FROM revision_changes WHERE org = $1 AND notebook = $2 AND ` + column + ` = $3 AND seq <= $4`
		if line != "" {
			query += ` AND seq <@ $5::text::int8multirange`
		}
		return "(" + query + ")"
	}
	args := []any{n.s.db.Org, n.id, path, r.seq}
	if line != "" {
		args = append(args, line)
	}
	var seq *int64
	if err := pool.QueryRow(ctx, `SELECT greatest(`+newest("path")+`, `+newest("old_path")+`)`, args...).Scan(&seq); err != nil || seq == nil {
		return gitobj.ID{}, err
	}
	ids, err := n.commitIDs(ctx, pool, []int64{*seq})
	return ids[*seq], err
}

// ReadBlobs calls fn with the content of each of blobs in turn. Every blob
// must be one the notebook holds or held, which is checked for all of them
// at once before any is read.
func (n *Notebook) ReadBlobs(ctx context.Context, blobs []gitobj.ID, fn func(i int, content []byte) error) error {
	if len(blobs) == 0 {
		return nil
	}
	m, err := n.current(ctx)
	if err != nil {
		return err
	}
	held := map[gitobj.ID]bool{}
	for _, f := range m.files {
		held[f.Blob] = true
	}
	var rest [][]byte
	for _, b := range blobs {
		if !held[b] {
			rest = append(rest, bytesOf(b))
		}
	}
	if len(rest) > 0 {
		rows, err := n.s.db.Pool.Query(ctx, `SELECT DISTINCT blob_sha FROM revision_changes WHERE org = $1 AND blob_sha = ANY($2) AND notebook = $3`, n.s.db.Org, rest, n.id)
		if err != nil {
			return err
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return err
		}
		for _, b := range found {
			held[toID(b)] = true
		}
		for _, b := range blobs {
			if !held[b] {
				return fmt.Errorf("content %s: %w", b, fs.ErrNotExist)
			}
		}
	}
	for i, b := range blobs {
		content, err := n.s.readContent(ctx, n.s.db.Pool, b)
		if err != nil {
			return err
		}
		if err := fn(i, content); err != nil {
			return err
		}
	}
	return nil
}
