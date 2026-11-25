package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/webcore"
)

// Sessions of MCP clients older than protocol 2026-07-28 are mcp_sessions rows,
// so any gateway serves each request of a session and a deploy keeps its
// client. The SDK's stateful handler still serves their requests: each gateway
// maps a stored session to an SDK session of its own and recreates a missing
// one from the client's initialize parameters. Resource subscriptions belong to
// the row, and the session's standalone GET stream, on whichever gateway the
// client reaches, delivers their notifications. A client's cancellation is
// announced to every gateway, and the one running the request it names cancels
// it there.
const (
	mcpSessionHeader = "Mcp-Session-Id"
	// mcpSessionKey carries the stored session's digest to MCP handlers. It is
	// removed from every incoming request.
	mcpSessionKey  = "X-Forgetmenote-Mcp-Session"
	mcpSessionIdle = 5 * time.Minute
	// A credential keeps at most this many live sessions; the one idle longest
	// gives way to a new one.
	mcpSessionsPerKey = 64
	mcpSubscriptions  = 32
	// mcpCancellations carries "<session digest> <request ID as JSON>" for each
	// cancellation a legacy client sends.
	mcpCancellations = "mcp_cancellations"
	// A request ID longer than this, as JSON, is cancelled only on the gateway
	// that receives the cancellation; it keeps the announcement well inside
	// Postgres's 8000-byte payload limit.
	mcpCancelledIDBytes = 1 << 10
)

var (
	errMCPSessionGone = errors.New("session not found")
	errMCPSessionUser = errors.New("session user mismatch")
	// The subscription hub's limits are capacity, which a client may retry
	// once it has unsubscribed or another session has ended.
	errMCPSessionFull       = webcore.Error{Status: http.StatusTooManyRequests, Code: "capacity", Message: "subscribe to at most 32 projects per session"}
	errSubscriptionSessions = webcore.Error{Status: http.StatusTooManyRequests, Code: "capacity", Message: "subscription session limit reached"}
)

type mcpSession struct {
	id            string // digest of the session ID
	version       int64
	initialize    []byte // the client's initialize parameters
	subscriptions map[string]revisionLease
}

type storedLease struct {
	User string `json:"user"`
	Key  string `json:"key"`
}

func openSubscriptions(raw []byte) (map[string]revisionLease, error) {
	var stored map[string]storedLease
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	out := make(map[string]revisionLease, len(stored))
	for uri, lease := range stored {
		out[uri] = revisionLease{lease.User, lease.Key}
	}
	return out, nil
}

func idleMillis() int64 { return mcpSessionIdle.Milliseconds() }

// beginMCPSession stores a new session bound to subject and returns its ID.
func (s *Server) beginMCPSession(ctx context.Context, subject string, initialize []byte) (string, error) {
	id := RandomToken()
	return id, pgx.BeginFunc(ctx, s.db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('mcp-sessions ' || $1 || ' ' || $2, 0))`, s.db.Org, subject); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM mcp_sessions WHERE org = $1 AND expires <= now()`, s.db.Org); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM mcp_sessions WHERE (org, id) IN (
			SELECT org, id FROM mcp_sessions WHERE org = $1 AND subject = $2 ORDER BY expires DESC OFFSET $3)`, s.db.Org, subject, mcpSessionsPerKey-1); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO mcp_sessions (org, id, subject, version, initialize, subscriptions, expires) VALUES ($1, $2, $3, 1, $4, '{}', now() + $5 * interval '1 millisecond')`,
			s.db.Org, digest(id), subject, initialize, idleMillis())
		return err
	})
}

// mcpSession returns the live session id names, which must be bound to
// subject, and extends its idle expiry at most once a minute.
func (s *Server) mcpSession(ctx context.Context, id, subject string) (mcpSession, error) {
	session := mcpSession{id: digest(id)}
	var owner string
	var raw []byte
	var fresh bool
	err := s.db.Pool.QueryRow(ctx, `SELECT subject, version, initialize, subscriptions, expires > now() + $3 * interval '1 millisecond' FROM mcp_sessions WHERE org = $1 AND id = $2 AND expires > now()`,
		s.db.Org, session.id, idleMillis()-time.Minute.Milliseconds()).Scan(&owner, &session.version, &session.initialize, &raw, &fresh)
	if errors.Is(err, pgx.ErrNoRows) {
		return session, errMCPSessionGone
	}
	if err != nil {
		return session, err
	}
	if owner != subject {
		return session, errMCPSessionUser
	}
	if session.subscriptions, err = openSubscriptions(raw); err != nil {
		return session, err
	}
	if !fresh {
		_, err = s.db.Pool.Exec(ctx, `UPDATE mcp_sessions SET expires = now() + $3 * interval '1 millisecond' WHERE org = $1 AND id = $2`, s.db.Org, session.id, idleMillis())
	}
	return session, err
}

// liveMCPSessions returns the live sessions among ids, digests, and keeps them
// from idling out: a session with an open stream is in use.
func (s *Server) liveMCPSessions(ctx context.Context, ids []string) (map[string]mcpSession, error) {
	rows, err := s.db.Pool.Query(ctx, `UPDATE mcp_sessions SET expires = GREATEST(expires, now() + $3 * interval '1 millisecond')
		WHERE org = $1 AND id = ANY($2) AND expires > now() RETURNING id, version, subscriptions`, s.db.Org, ids, idleMillis())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]mcpSession{}
	for rows.Next() {
		var session mcpSession
		var raw []byte
		if err = rows.Scan(&session.id, &session.version, &raw); err != nil {
			return nil, err
		}
		if session.subscriptions, err = openSubscriptions(raw); err != nil {
			return nil, err
		}
		out[session.id] = session
	}
	return out, rows.Err()
}

func (s *Server) endMCPSession(ctx context.Context, id string) error {
	_, err := s.db.Pool.Exec(ctx, `DELETE FROM mcp_sessions WHERE org = $1 AND id = $2`, s.db.Org, id)
	return err
}

func (s *Server) subscribeMCPSession(ctx context.Context, id, uri string, lease revisionLease) error {
	stored, err := json.Marshal(storedLease{lease.user, lease.key})
	if err != nil {
		return err
	}
	tag, err := s.db.Pool.Exec(ctx, `UPDATE mcp_sessions SET subscriptions = subscriptions || jsonb_build_object($3::text, $4::jsonb), version = version + 1
		WHERE org = $1 AND id = $2 AND expires > now() AND (subscriptions ? $3 OR (SELECT count(*) FROM jsonb_object_keys(subscriptions)) < $5)`,
		s.db.Org, id, uri, string(stored), mcpSubscriptions)
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	var live bool
	if err = s.db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT FROM mcp_sessions WHERE org = $1 AND id = $2 AND expires > now())`, s.db.Org, id).Scan(&live); err != nil {
		return err
	}
	if !live {
		return errMCPSessionGone
	}
	return errMCPSessionFull
}

func (s *Server) unsubscribeMCPSession(ctx context.Context, id, uri string) error {
	_, err := s.db.Pool.Exec(ctx, `UPDATE mcp_sessions SET subscriptions = subscriptions - $3, version = version + 1 WHERE org = $1 AND id = $2 AND subscriptions ? $3`, s.db.Org, id, uri)
	return err
}

// legacyMCPSession names the stored session a request belongs to, as a
// digest, or "" for a current-protocol request or one without a session.
func legacyMCPSession(header http.Header) string {
	if header == nil {
		return ""
	}
	return header.Get(mcpSessionKey)
}

// initializeParams returns the parameters of an initialize request. It reads
// at most 64 KiB of the body, which it leaves for the handler to read again.
func initializeParams(r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, false
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	var message struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err != nil || len(head) > 64<<10 || json.Unmarshal(head, &message) != nil || message.Method != "initialize" || len(message.Params) == 0 {
		return nil, false
	}
	return message.Params, true
}

// legacySessions maps stored sessions to the SDK sessions serving them on this
// gateway.
type legacySessions struct {
	app     *Server
	server  *mcp.Server
	handler http.Handler // the SDK's stateful handler
	mu      sync.Mutex
	local   map[string]localSession // by stored session digest
	restore sync.Mutex
	// announced is the SDK's handler for the cancellations this gateway hears.
	announced     http.Handler
	stopListening context.CancelFunc
	listening     sync.WaitGroup
}

// A localSession is the SDK session serving a stored session on this gateway
// and the token user the SDK serves it to.
type localSession struct {
	session *mcp.ServerSession
	user    string
}

type announcedUser struct{}

func (l *legacySessions) lookup(id string) *mcp.ServerSession {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.local[id].session
}

// bind records the initialized SDK session named local as id's on this gateway
// until the SDK closes it.
func (l *legacySessions) bind(id, local, user string) bool {
	var found *mcp.ServerSession
	for session := range l.server.Sessions() {
		if session.ID() == local {
			found = session
			break
		}
	}
	if found == nil || found.InitializeParams() == nil {
		return false
	}
	l.mu.Lock()
	l.local[id] = localSession{found, user}
	l.mu.Unlock()
	go func() {
		found.Wait()
		l.mu.Lock()
		if l.local[id].session == found {
			delete(l.local, id)
		}
		l.mu.Unlock()
	}()
	return true
}

// close ends id's SDK session on this gateway, if it has one.
func (l *legacySessions) close(id string) {
	l.mu.Lock()
	session := l.local[id].session
	delete(l.local, id)
	l.mu.Unlock()
	if session != nil {
		session.Close()
	}
}

// announceCancel tells every gateway that the client of stored session id
// cancelled request, so the one running it cancels it. Only the session's
// digest and the request ID travel.
func (l *legacySessions) announceCancel(id string, request any) {
	switch request.(type) {
	case string, float64:
	default:
		return // The SDK cancels only string and number IDs.
	}
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > mcpCancelledIDBytes {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	l.app.db.Announce(ctx, mcpCancellations, id+" "+string(raw))
}

// listen hands this gateway's SDK sessions the cancellations their clients
// send through any gateway, until closeListener. A gateway hears nothing
// announced while it is not listening, as while it reconnects, so a call it
// runs then misses that cancellation and runs to its end.
func (l *legacySessions) listen() {
	// The SDK serves a session only to the token user that began it; heard
	// cancellations carry that user, never a credential.
	l.announced = auth.RequireBearerToken(func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		user, ok := ctx.Value(announcedUser{}).(string)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: user, Expiration: time.Now().Add(time.Minute)}, nil
	}, nil)(l.handler)
	ctx, cancel := context.WithCancel(context.Background())
	l.stopListening = cancel
	l.listening.Add(1)
	go func() {
		defer l.listening.Done()
		l.app.db.Listen(ctx, mcpCancellations, l.cancelHeard)
	}()
}

func (l *legacySessions) closeListener() {
	if l.stopListening != nil {
		l.stopListening()
		l.listening.Wait()
	}
}

// cancelHeard posts an announced cancellation to the SDK session serving its
// stored session here, which cancels the request if it runs it.
func (l *legacySessions) cancelHeard(announced string) {
	id, raw, ok := strings.Cut(announced, " ")
	if !ok {
		return // Listening began; what it missed is lost.
	}
	l.mu.Lock()
	local, held := l.local[id]
	l.mu.Unlock()
	var request any
	if !held || json.Unmarshal([]byte(raw), &request) != nil {
		return
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": request}})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), announcedUser{}, local.user), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/mcp", bytes.NewReader(body))
	if err != nil {
		return
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	// The SDK's bearer middleware requires a token, which l.announced ignores.
	r.Header.Set("Authorization", "Bearer announced")
	r.Header.Set(mcpSessionHeader, local.session.ID())
	l.announced.ServeHTTP(&recorder{header: http.Header{}}, r)
}

// replay builds an internal request that carries r's credentials.
func replay(r *http.Request, body, session string) *http.Request {
	c := r.Clone(r.Context())
	c.Method = http.MethodPost
	c.Body = io.NopCloser(strings.NewReader(body))
	c.ContentLength = int64(len(body))
	c.Header.Del("Content-Length")
	c.Header.Set("Content-Type", "application/json")
	c.Header.Set("Accept", "application/json, text/event-stream")
	c.Header.Del(mcpSessionHeader)
	if session != "" {
		c.Header.Set(mcpSessionHeader, session)
	}
	return c
}

// recorder keeps the status and headers of a replayed request.
type recorder struct {
	header http.Header
	code   int
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}
func (r *recorder) Write(p []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return len(p), nil
}

// ensure returns this gateway's SDK session for session, recreating it from the
// client's stored initialization when it has none, as after a deploy or when
// the client reaches another gateway. user is r's token user, to whom the SDK
// serves the session.
func (l *legacySessions) ensure(r *http.Request, session mcpSession, user string) (*mcp.ServerSession, int) {
	if local := l.lookup(session.id); local != nil {
		return local, 0
	}
	l.restore.Lock()
	defer l.restore.Unlock()
	if local := l.lookup(session.id); local != nil {
		return local, 0
	}
	initialize := &recorder{header: http.Header{}}
	l.handler.ServeHTTP(initialize, replay(r, `{"jsonrpc":"2.0","id":"restore","method":"initialize","params":`+string(session.initialize)+`}`, ""))
	local := initialize.header.Get(mcpSessionHeader)
	if initialize.code != http.StatusOK || local == "" {
		if initialize.code == http.StatusTooManyRequests {
			return nil, initialize.code
		}
		return nil, http.StatusNotFound
	}
	initialized := &recorder{header: http.Header{}}
	l.handler.ServeHTTP(initialized, replay(r, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, local))
	if initialized.code != http.StatusAccepted || !l.bind(session.id, local, user) {
		for s := range l.server.Sessions() {
			if s.ID() == local {
				s.Close()
			}
		}
		return nil, http.StatusNotFound
	}
	return l.lookup(session.id), 0
}

// initializeWriter hands the client the stored session's ID in place of the
// SDK's, and binds the SDK session before the response is sent.
type initializeWriter struct {
	http.ResponseWriter
	outward string
	bind    func(local string) bool
	bound   bool
	settled bool
}

func (w *initializeWriter) settle() {
	if w.settled {
		return
	}
	w.settled = true
	if local := w.Header().Get(mcpSessionHeader); local != "" && w.bind(local) {
		w.bound = true
		w.Header().Set(mcpSessionHeader, w.outward)
		return
	}
	w.Header().Del(mcpSessionHeader)
}
func (w *initializeWriter) WriteHeader(code int) {
	w.settle()
	w.ResponseWriter.WriteHeader(code)
}
func (w *initializeWriter) Write(p []byte) (int, error) {
	w.settle()
	return w.ResponseWriter.Write(p)
}
func (w *initializeWriter) Flush() {
	w.settle()
	http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *initializeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// serve serves a request of a client older than protocol 2026-07-28.
func (l *legacySessions) serve(w http.ResponseWriter, r *http.Request) {
	s := l.app
	outward := r.Header.Get(mcpSessionHeader)
	var bound string
	if info := auth.TokenInfoFromContext(r.Context()); info != nil {
		bound = info.UserID
	}
	if r.Method == http.MethodPost && outward == "" {
		params, ok := initializeParams(r)
		if !ok {
			l.handler.ServeHTTP(w, r)
			return
		}
		id, err := s.beginMCPSession(r.Context(), bound, params)
		if err != nil {
			http.Error(w, "MCP sessions are unavailable; retry shortly", 503)
			return
		}
		writer := &initializeWriter{ResponseWriter: w, outward: id, bind: func(local string) bool { return l.bind(digest(id), local, bound) }}
		l.handler.ServeHTTP(writer, r)
		if !writer.bound {
			s.endMCPSession(context.WithoutCancel(r.Context()), digest(id))
		}
		return
	}
	switch r.Method {
	case http.MethodPost, http.MethodGet, http.MethodDelete:
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if outward == "" {
		http.Error(w, "Bad Request: "+r.Method+" requires an Mcp-Session-Id header", 400)
		return
	}
	if r.Method == http.MethodGet && !acceptsEvents(r.Header.Values("Accept")) {
		http.Error(w, "Accept must contain 'text/event-stream' for GET requests", 400)
		return
	}
	session, err := s.mcpSession(r.Context(), outward, bound)
	switch {
	case errors.Is(err, errMCPSessionGone):
		l.close(digest(outward))
		http.Error(w, "session not found", http.StatusNotFound)
		return
	case errors.Is(err, errMCPSessionUser):
		http.Error(w, "session user mismatch", http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, "MCP sessions are unavailable; retry shortly", 503)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.revisions.stream(w, r, session)
	case http.MethodDelete:
		if err = s.endMCPSession(r.Context(), session.id); err != nil {
			http.Error(w, "MCP sessions are unavailable; retry shortly", 503)
			return
		}
		l.close(session.id)
		s.revisions.endStreams(session.id)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPost:
		local, code := l.ensure(r, session, bound)
		if local == nil {
			http.Error(w, "session not found", code)
			return
		}
		r.Header.Set(mcpSessionHeader, local.ID())
		r.Header.Set(mcpSessionKey, session.id)
		l.handler.ServeHTTP(w, r)
	}
}

func acceptsEvents(values []string) bool {
	for _, value := range values {
		for _, raw := range strings.Split(value, ",") {
			base, _, _ := strings.Cut(strings.TrimSpace(raw), ";")
			switch strings.ToLower(strings.TrimSpace(base)) {
			case "text/event-stream", "text/*", "*/*":
				return true
			}
		}
	}
	return false
}
