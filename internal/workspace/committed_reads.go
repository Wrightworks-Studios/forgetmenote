package workspace

import (
	"bytes"
	"sync"

	"forgetmenote/internal/gitobj"
)

// One note open asks for the same committed file several times: the note
// read, its live snapshot and its comments, then every live poll. A commit and
// path name immutable content within one incarnation of the notebook's
// history, so writes, handoffs and restores need no invalidation. A purge,
// on any gateway, rewrites the history and starts a new incarnation: the
// entries of an older one are never served again and are dropped, so a read
// pinned to a rewritten revision cannot return what the purge removed. The
// cache is small and per notebook; larger files are always read from the
// notes store.
const (
	committedReadEntries = 32
	committedReadBytes   = 1 << 20
)

type committedReads struct {
	mu      sync.Mutex
	inc     int64           // the incarnation the entries and rewrites are of
	entries []committedRead // least recently used first
	bytes   int
	// rewrites maps the revisions purges rewrote to those standing for
	// them in incarnation inc, once loaded (purgeRewrites).
	rewrites map[gitobj.ID]gitobj.ID
}

type committedRead struct {
	document Document
	found    bool
}

// at reports whether what c holds is of incarnation inc, dropping it first
// when inc is newer. Callers hold c.mu.
func (c *committedReads) at(inc int64) bool {
	if inc > c.inc {
		c.inc, c.entries, c.bytes, c.rewrites = inc, nil, 0, nil
	}
	return inc == c.inc
}

// get returns a private copy, so callers cannot change later reads.
func (c *committedReads) get(inc int64, path, revision string) (Document, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at(inc) {
		return Document{}, false, false
	}
	for i, entry := range c.entries {
		if entry.document.Path == path && entry.document.Revision == revision {
			copy(c.entries[i:], c.entries[i+1:])
			c.entries[len(c.entries)-1] = entry
			document := entry.document
			document.Content = bytes.Clone(document.Content)
			return document, entry.found, true
		}
	}
	return Document{}, false, false
}

// put keeps what reading path at revision gave in incarnation inc, the one
// the read began in.
func (c *committedReads) put(inc int64, path, revision string, document Document, found bool) {
	if len(document.Content) > committedReadBytes {
		return
	}
	document.Path, document.Revision = path, revision
	document.Content = bytes.Clone(document.Content)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at(inc) {
		return
	}
	for _, entry := range c.entries {
		if entry.document.Path == path && entry.document.Revision == revision {
			return
		}
	}
	c.entries = append(c.entries, committedRead{document: document, found: found})
	c.bytes += len(document.Content)
	for len(c.entries) > committedReadEntries || c.bytes > committedReadBytes {
		c.bytes -= len(c.entries[0].document.Content)
		c.entries[0] = committedRead{}
		c.entries = c.entries[1:]
	}
}

// purgeRewrites returns the purge mapping of incarnation inc, if loaded.
func (c *committedReads) purgeRewrites(inc int64) (map[gitobj.ID]gitobj.ID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at(inc) || c.rewrites == nil {
		return nil, false
	}
	return c.rewrites, true
}

// keepRewrites keeps rewrites, loaded in incarnation inc, which callers
// must not change.
func (c *committedReads) keepRewrites(inc int64, rewrites map[gitobj.ID]gitobj.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at(inc) {
		c.rewrites = rewrites
	}
}
