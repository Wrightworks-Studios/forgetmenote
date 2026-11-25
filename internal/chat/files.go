package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

// The file-based release kept each conversation in
// chat/users/<account>/conversations/<id>/state.json, account being the
// SHA-256 of the owner, beside the recovery.json overlay that interrupts runs
// of a record too full to take the interruption. chat/.lock only marked the
// gateway that owned the directory, and a .chat-* file is a temporary that a
// crash left before its rename; that release reads neither, so neither moves.
const (
	chatFiles    = "chat"
	recordFile   = "state.json"
	recoveryFile = "recovery.json"
)

var accountID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func conversationDir(account, id string) string {
	return path.Join(chatFiles, "users", account, "conversations", id)
}

type conversationKey struct{ account, id string }

// ImportFiles copies every conversation's record and recovery overlay, byte for
// byte, into chat_conversations rows at version 1, checking each as the
// file-based release does when it reads it. That release shows a record it
// cannot read as needing restoration from a backup, which works only while it
// is a file, so the import refuses one instead.
//
// A run that release left queued or running, or with tokens in flight, names
// no gateway, so the first store to open the database interrupts it as that
// release would have on its next start. The import keeps the record's bytes, so
// an overlay bound to them still applies, and flags it active as the store's
// own write would, so admission waits for that recovery.
func ImportFiles(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source) (filestate.Sum, error) {
	var tally filestate.Tally
	keys, err := conversationKeys(src)
	if err != nil {
		return filestate.Sum{}, err
	}
	if len(keys) == 0 {
		return tally.Sum(), nil
	}
	// Each record is read as it is copied, so the import holds one at a time
	// however many conversations there are. The copy ends before CopyFrom
	// returns, so readErr and tally are settled by then.
	var readErr error
	next := 0
	rows := pgx.CopyFromFunc(func() ([]any, error) {
		if next == len(keys) {
			return nil, nil
		}
		key := keys[next]
		next++
		record, recovery, active, err := readConversation(src, key, &tally)
		if err != nil {
			readErr = err
			return nil, err
		}
		return []any{db.Org, key.account, key.id, int64(1), active, record, recovery}, nil
	})
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"chat_conversations"}, []string{"org", "account", "id", "version", "active", "record", "recovery"}, rows); readErr != nil {
		// Postgres reports a refused file only as a failed COPY.
		err = readErr
	}
	if err != nil {
		return filestate.Sum{}, err
	}
	return tally.Sum(), nil
}

// conversationKeys lists the conversation directories, refusing anything else
// under chat/ so that no file stays behind unread.
func conversationKeys(src *filestate.Source) ([]conversationKey, error) {
	top, err := src.Entries(".")
	if err != nil {
		return nil, err
	}
	for _, e := range top {
		if e.Name() == chatFiles && !e.IsDir() {
			return nil, errors.New("chat is not a directory, which the file-based release refuses to open")
		}
	}
	if _, err = children(src, chatFiles, func(e fs.DirEntry) bool {
		return e.Name() == ".lock" && e.Type().IsRegular() || e.Name() == "users" && e.IsDir()
	}); err != nil {
		return nil, err
	}
	users := path.Join(chatFiles, "users")
	accounts, err := children(src, users, func(e fs.DirEntry) bool { return e.IsDir() && accountID.MatchString(e.Name()) })
	if err != nil {
		return nil, err
	}
	var keys []conversationKey
	for _, account := range accounts {
		dir := path.Join(users, account)
		if _, err = children(src, dir, func(e fs.DirEntry) bool { return e.Name() == "conversations" && e.IsDir() }); err != nil {
			return nil, err
		}
		ids, err := children(src, path.Join(dir, "conversations"), func(e fs.DirEntry) bool { return e.IsDir() && chatID.MatchString(e.Name()) })
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			keys = append(keys, conversationKey{account, id})
		}
	}
	return keys, nil
}

// children lists a directory's names, refusing an entry that keep rejects.
func children(src *filestate.Source, dir string, keep func(fs.DirEntry) bool) ([]string, error) {
	entries, err := src.Entries(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !keep(e) {
			return nil, fmt.Errorf("%s is not chat state the file-based release reads; move it out of the data directory, then import", path.Join(dir, e.Name()))
		}
		names = append(names, e.Name())
	}
	return names, nil
}

// readConversation reads and tallies one conversation's files, and reports
// whether the store would flag the record active.
func readConversation(src *filestate.Source, key conversationKey, tally *filestate.Tally) (record, recovery []byte, active bool, err error) {
	dir := conversationDir(key.account, key.id)
	names, err := children(src, dir, func(e fs.DirEntry) bool {
		return e.Name() == recordFile || e.Name() == recoveryFile || strings.HasPrefix(e.Name(), ".chat-") && e.Type().IsRegular()
	})
	if err != nil {
		return nil, nil, false, err
	}
	refuse := func(reason string) error {
		return fmt.Errorf("%s %s; restore that conversation from a backup or remove its directory, then import", dir, reason)
	}
	if !slices.Contains(names, recordFile) {
		return nil, nil, false, refuse("has no " + recordFile)
	}
	if record, err = src.Read(path.Join(dir, recordFile)); err != nil {
		return nil, nil, false, err
	}
	if slices.Contains(names, recoveryFile) {
		if recovery, err = src.Read(path.Join(dir, recoveryFile)); err != nil {
			return nil, nil, false, err
		}
	}
	var meta struct{ Owner, Project string }
	if json.Unmarshal(record, &meta) != nil {
		return nil, nil, false, refuse("holds an unreadable record")
	}
	if Hash([]byte(meta.Owner)) != key.account {
		return nil, nil, false, refuse("holds a record of another account")
	}
	var s Store
	c, err := s.decode(meta.Owner, meta.Project, key.id, record, recovery)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, false, refuse("holds a record of another conversation or an unknown version")
	}
	if err != nil {
		return nil, nil, false, refuse("cannot be read (" + err.Error() + ")")
	}
	for _, r := range c.Runs {
		// The file-based release's strict reader refuses the field.
		if r.Holder != "" {
			return nil, nil, false, refuse("holds a run that names a gateway")
		}
	}
	tally.Add(path.Join(dir, recordFile), record)
	if recovery != nil {
		tally.Add(path.Join(dir, recoveryFile), recovery)
	}
	return record, recovery, activeRuns(c), nil
}

// ExportFiles writes every conversation's record and recovery overlay at the
// paths the file-based release reads. The row version and active flag stay
// behind: that release counts stream generations in memory from zero and
// rebuilds its admission index from the records when it opens.
func ExportFiles(ctx context.Context, db *pg.DB, q pg.Querier, out *filestate.Target) (filestate.Sum, error) {
	var tally filestate.Tally
	rows, err := q.Query(ctx, `SELECT account, id, record, recovery FROM chat_conversations WHERE org = $1 ORDER BY account, id`, db.Org)
	if err != nil {
		return filestate.Sum{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var account, id string
		var record, recovery []byte
		if err = rows.Scan(&account, &id, &record, &recovery); err != nil {
			return filestate.Sum{}, err
		}
		if !accountID.MatchString(account) || !chatID.MatchString(id) {
			return filestate.Sum{}, errors.New("a chat conversation is stored under an invalid key")
		}
		if record, recovery, err = withoutHolders(record, recovery); err != nil {
			return filestate.Sum{}, err
		}
		dir := conversationDir(account, id)
		if err = out.Write(path.Join(dir, recordFile), record); err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(path.Join(dir, recordFile), record)
		if recovery != nil {
			if err = out.Write(path.Join(dir, recoveryFile), recovery); err != nil {
				return filestate.Sum{}, err
			}
			tally.Add(path.Join(dir, recoveryFile), recovery)
		}
	}
	if err = rows.Err(); err != nil {
		return filestate.Sum{}, err
	}
	return tally.Sum(), nil
}

// withoutHolders drops the gateway each run names, a field the file-based
// release's strict reader refuses. No gateway serves during an export, so a run
// left queued or running is written as that release leaves one when it stops
// abruptly, and its next start interrupts it. A rewritten record takes the
// recovery overlay bound to its old bytes along to its new ones. A record the
// store cannot read stays as it is, which that release reads alike.
func withoutHolders(record, recovery []byte) ([]byte, []byte, error) {
	if !bytes.Contains(record, []byte(`"holder":`)) {
		return record, recovery, nil
	}
	var meta struct{ Owner, Project, ID string }
	if json.Unmarshal(record, &meta) != nil {
		return record, recovery, nil
	}
	var s Store
	c, err := s.decode(meta.Owner, meta.Project, meta.ID, record, nil)
	if err != nil {
		return record, recovery, nil
	}
	held := false
	for i := range c.Runs {
		held = held || c.Runs[i].Holder != ""
		c.Runs[i].Holder = ""
	}
	if !held {
		return record, recovery, nil
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, nil, err
	}
	var overlay recoveryRecord
	if recovery != nil && json.Unmarshal(recovery, &overlay) == nil && overlay.Hash == Hash(record) {
		overlay.Hash = Hash(raw)
		if recovery, err = json.Marshal(overlay); err != nil {
			return nil, nil, err
		}
	}
	return raw, recovery, nil
}
