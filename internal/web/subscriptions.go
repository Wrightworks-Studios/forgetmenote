package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type leaseKey struct{}
type revisionLease struct{ user, key string }
type projectWatch struct {
	cancel func()
	stop   chan struct{}
}

// One bounded hint channel per watched project. Git, not this hub, owns catch-up.
// Current-protocol subscriptions belong to their SDK session; a legacy
// session's belong to its stored row, and its GET stream here delivers them.
type revisionHub struct {
	mu       sync.Mutex
	app      *Server
	server   *mcp.Server
	sessions map[*mcp.ServerSession]map[string]revisionLease
	streams  map[*legacyStream]struct{}
	projects map[string]*projectWatch
	stop     chan struct{}
	once     sync.Once
	wg       sync.WaitGroup
	closed   bool
}

// A legacyStream is a stored session's standalone GET stream on this gateway.
type legacyStream struct {
	session       string // digest of the session ID
	version       int64
	subscriptions map[string]revisionLease
	events        chan string
	done          chan struct{}
	// retry asks the client to reconnect promptly, to another gateway when
	// this one is shutting down.
	retry bool
	// refused is why the stream cannot deliver one of its subscriptions,
	// which ended it.
	refused error
}

func (s *Server) newRevisionHub() *revisionHub {
	h := &revisionHub{app: s, sessions: map[*mcp.ServerSession]map[string]revisionLease{}, streams: map[*legacyStream]struct{}{}, projects: map[string]*projectWatch{}, stop: make(chan struct{})}
	h.wg.Add(1)
	go h.expire()
	return h
}
func revisionProject(uri string) string {
	uri = strings.Replace(uri, "forgetmenote://notebooks/", "forgetmenote://projects/", 1)
	if !strings.HasPrefix(uri, "forgetmenote://projects/") || !strings.HasSuffix(uri, "/revision") {
		return ""
	}
	id := strings.TrimSuffix(strings.TrimPrefix(uri, "forgetmenote://projects/"), "/revision")
	if id == "" || strings.ContainsAny(id, "/?#%") {
		return ""
	}
	return id
}
func (h *revisionHub) valid(lease revisionLease, project string) bool {
	s := h.app
	if lease.key != digest(s.token) {
		if family, oauth := strings.CutPrefix(lease.key, "grant:"); oauth {
			g, ok := s.oauth.FamilyGrant(family)
			if !ok || g.Subject != lease.user || g.Resource != s.origin+"/mcp" || !slices.Contains(strings.Fields(g.Scope), "notes:read") || !s.catalog.ValidSession(g.Subject, g.Epoch) {
				return false
			}
		} else {
			credential, ok := s.oauth.Credential(lease.key)
			if !ok || credential.Subject != lease.user || credential.Expires <= time.Now().Unix() || !slices.Contains(credential.Audiences, "mcp") || !slices.Contains(credential.Scopes, "notes:read") || !s.catalog.ValidSession(credential.Subject, credential.Epoch) || (!credential.AllProjects && !slices.Contains(credential.Projects, project)) {
				return false
			}
		}
	}
	_, err := s.catalog.Access(lease.user, project, "read")
	return err == nil
}
func (h *revisionHub) subscribe(ctx context.Context, req *mcp.SubscribeRequest) error {
	project := revisionProject(req.Params.URI)
	store, err := h.app.toolStore(ctx, project, "read")
	if err != nil {
		return workspace.ErrForbidden
	}
	lease, ok := ctx.Value(leaseKey{}).(revisionLease)
	if !ok || !h.valid(lease, project) {
		return workspace.ErrForbidden
	}
	if req.Extra != nil && req.Extra.Header.Get("MCP-Protocol-Version") < "2026-07-28" {
		id := legacyMCPSession(req.Extra.Header)
		if id == "" {
			return errors.New("resource subscriptions need an MCP session; initialize one first")
		}
		if err = h.app.subscribeMCPSession(ctx, id, req.Params.URI, lease); err != nil {
			return err
		}
		h.syncStreams(ctx, id)
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("server is shutting down")
	}
	subscriptions := h.sessions[req.Session]
	if subscriptions == nil {
		if len(h.sessions) >= 128 {
			return errSubscriptionSessions
		}
		subscriptions = map[string]revisionLease{}
		h.sessions[req.Session] = subscriptions
		go func() { req.Session.Wait(); h.remove(req.Session, "") }()
	}
	if len(subscriptions) >= 32 {
		return errMCPSessionFull
	}
	if err := h.watchLocked(project, store); err != nil {
		return err
	}
	subscriptions[req.Params.URI] = lease
	return nil
}

// watchLocked starts project's watch unless one runs. h.mu is held.
func (h *revisionHub) watchLocked(project string, store *workspace.Store) error {
	if h.projects[project] != nil {
		return nil
	}
	hints, cancel, err := store.Watch()
	if err != nil {
		return err
	}
	watch := &projectWatch{cancel: cancel, stop: make(chan struct{})}
	h.projects[project] = watch
	h.wg.Add(1)
	go h.deliver(project, store, hints, watch.stop)
	return nil
}

// releaseLocked stops the watches no session or stream uses. h.mu is held.
func (h *revisionHub) releaseLocked() {
	used := map[string]bool{}
	for _, subs := range h.sessions {
		for uri := range subs {
			used[revisionProject(uri)] = true
		}
	}
	for stream := range h.streams {
		for uri := range stream.subscriptions {
			used[revisionProject(uri)] = true
		}
	}
	for id, watch := range h.projects {
		if !used[id] {
			watch.cancel()
			close(watch.stop)
			delete(h.projects, id)
		}
	}
}
func (h *revisionHub) remove(session *mcp.ServerSession, uri string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if uri == "" {
		delete(h.sessions, session)
	} else {
		delete(h.sessions[session], uri)
	}
	h.releaseLocked()
}
func (h *revisionHub) unsubscribe(ctx context.Context, req *mcp.UnsubscribeRequest) error {
	if revisionProject(req.Params.URI) == "" {
		return workspace.ErrForbidden
	}
	if req.Extra != nil {
		if id := legacyMCPSession(req.Extra.Header); id != "" {
			if err := h.app.unsubscribeMCPSession(ctx, id, req.Params.URI); err != nil {
				return err
			}
			h.syncStreams(ctx, id)
			return nil
		}
	}
	h.remove(req.Session, req.Params.URI)
	return nil
}

// endLocked closes stream once. h.mu is held.
func (h *revisionHub) endLocked(stream *legacyStream, retry bool) {
	if _, ok := h.streams[stream]; !ok {
		return
	}
	delete(h.streams, stream)
	stream.retry = retry
	close(stream.done)
}

// endStreams closes this gateway's streams of an ended session.
func (h *revisionHub) endStreams(session string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for stream := range h.streams {
		if stream.session == session {
			h.endLocked(stream, false)
		}
	}
	h.releaseLocked()
}

// syncStreams rereads the sessions of this gateway's streams, or of only one
// session, so a subscription made through any gateway reaches the stream here.
// A stream whose session has ended closes.
func (h *revisionHub) syncStreams(ctx context.Context, only string) {
	h.mu.Lock()
	var ids []string
	for stream := range h.streams {
		if only == "" || stream.session == only {
			ids = append(ids, stream.session)
		}
	}
	h.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	sessions, err := h.app.liveMCPSessions(ctx, ids)
	if err != nil {
		// Each delivery still checks its lease against current authority.
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for stream := range h.streams {
		if only != "" && stream.session != only {
			continue
		}
		current, ok := sessions[stream.session]
		if !ok {
			h.endLocked(stream, false)
			continue
		}
		if current.version == stream.version {
			continue
		}
		stream.version, stream.subscriptions = current.version, current.subscriptions
		for uri, lease := range stream.subscriptions {
			project := revisionProject(uri)
			// A lease that lost access is left to expire, which closes the session.
			store, err := h.app.catalog.Access(lease.user, project, "read")
			if err != nil {
				continue
			}
			// A stream that cannot watch a subscription would stay silent
			// about it; it ends and tells its client why.
			if err := h.watchLocked(project, store); err != nil {
				stream.refused = err
				h.endLocked(stream, false)
				break
			}
		}
	}
	h.releaseLocked()
}

// stream serves a legacy session's standalone GET stream: its subscriptions'
// notifications, as JSON-RPC messages, while the request lasts.
func (h *revisionHub) stream(w http.ResponseWriter, r *http.Request, session mcpSession) {
	stream := &legacyStream{session: session.id, events: make(chan string, 16), done: make(chan struct{})}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		http.Error(w, "server is shutting down", 503)
		return
	}
	// A client that reconnects replaces its earlier stream.
	for other := range h.streams {
		if other.session == session.id {
			h.endLocked(other, false)
		}
	}
	if len(h.streams) >= 128 {
		h.mu.Unlock()
		fail(w, errSubscriptionSessions)
		return
	}
	h.streams[stream] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.endLocked(stream, false)
		h.releaseLocked()
		h.mu.Unlock()
	}()
	// Subscribe before answering: the client sends its next request only after
	// this stream opens.
	h.syncStreams(r.Context(), session.id)
	h.mu.Lock()
	refused := stream.refused
	h.mu.Unlock()
	if refused != nil {
		fail(w, refused)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flush := http.NewResponseController(w)
	flush.Flush()
	// Event IDs let the client see progress; it reconnects without replay, as
	// Git owns catch-up.
	prefix, sent := RandomToken()[:12], 0
	streaming, stopStreaming := webcore.UntilDrain(r.Context())
	defer stopStreaming()
	for {
		select {
		case uri := <-stream.events:
			data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/resources/updated", "params": map[string]string{"uri": uri}})
			if err != nil {
				return
			}
			sent++
			if _, err = fmt.Fprintf(w, "event: message\nid: %s-%d\ndata: %s\n\n", prefix, sent, data); err != nil {
				return
			}
			flush.Flush()
		case <-stream.done:
			if stream.refused != nil {
				// A subscription made later, through any gateway, that this
				// stream cannot watch: its client hears why it ends.
				detail := webcore.ErrorOf(stream.refused, http.StatusInternalServerError)
				data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "error", "logger": "forgetmenote", "data": map[string]any{"status": detail.Status, "code": detail.Code, "message": detail.Message}}})
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
				flush.Flush()
			}
			if stream.retry {
				fmt.Fprint(w, "retry: 1000\ndata: \n\n")
				flush.Flush()
			}
			return
		case <-streaming.Done():
			if webcore.Drained(streaming) {
				// This gateway is stopping: its client reconnects promptly,
				// to another one.
				fmt.Fprint(w, "retry: 1000\ndata: \n\n")
				flush.Flush()
			}
			return
		}
	}
}

// notifyStreams hands project's change to the streams subscribed to it.
func (h *revisionHub) notifyStreams(project string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for stream := range h.streams {
		for uri, lease := range stream.subscriptions {
			if revisionProject(uri) == project && h.valid(lease, project) {
				select {
				case stream.events <- uri:
				default: // A hint already waits for this slow reader.
				}
			}
		}
	}
}
func (h *revisionHub) deliver(project string, store *workspace.Store, hints <-chan struct{}, stop <-chan struct{}) {
	defer h.wg.Done()
	refresh := time.NewTicker(5 * time.Second)
	defer refresh.Stop()
	for {
		select {
		case <-stop:
			return
		case <-h.stop:
			return
		case <-refresh.C:
			store.Status(context.Background()) // Same committed HEAD refresh as the browser.
		case _, ok := <-hints:
			if !ok {
				return
			}
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-h.stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			select {
			case <-hints:
			default:
			}
			h.server.ResourceUpdated(context.Background(), &mcp.ResourceUpdatedNotificationParams{URI: revisionURI(project)})
			h.server.ResourceUpdated(context.Background(), &mcp.ResourceUpdatedNotificationParams{URI: "forgetmenote://notebooks/" + project + "/revision"})
			h.notifyStreams(project)
		}
	}
}
func (h *revisionHub) expire() {
	defer h.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			var invalid []*mcp.ServerSession
			var ended []string
			h.mu.Lock()
			for session, subs := range h.sessions {
				for uri, lease := range subs {
					if !h.valid(lease, revisionProject(uri)) {
						invalid = append(invalid, session)
						break
					}
				}
			}
			// A stored session that lost a subscription's access ends, as an SDK
			// session closes.
			for stream := range h.streams {
				for uri, lease := range stream.subscriptions {
					if !h.valid(lease, revisionProject(uri)) {
						ended = append(ended, stream.session)
						h.endLocked(stream, false)
						break
					}
				}
			}
			h.releaseLocked()
			h.mu.Unlock()
			for _, session := range invalid {
				h.remove(session, "")
				session.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			for _, id := range ended {
				h.app.endMCPSession(ctx, id)
				if h.app.legacy != nil {
					h.app.legacy.close(id)
				}
			}
			h.syncStreams(ctx, "")
			cancel()
		}
	}
}
func (h *revisionHub) close() {
	h.once.Do(func() {
		h.mu.Lock()
		h.closed = true
		close(h.stop)
		// Stored sessions outlive this gateway; their clients reconnect elsewhere.
		for stream := range h.streams {
			h.endLocked(stream, true)
		}
		h.mu.Unlock()
		var sessions []*mcp.ServerSession
		for session := range h.server.Sessions() {
			sessions = append(sessions, session)
		}
		for _, session := range sessions {
			h.remove(session, "")
			session.Close()
		}
		h.wg.Wait()
	})
}
func (h *revisionHub) sending(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "notifications/resources/updated" {
			p := req.GetParams().(*mcp.ResourceUpdatedNotificationParams)
			session := req.GetSession().(*mcp.ServerSession)
			project := revisionProject(p.URI)
			h.mu.Lock()
			lease, ok := h.sessions[session][p.URI]
			h.mu.Unlock()
			if !ok || !h.valid(lease, project) {
				return nil, workspace.ErrForbidden
			}
			bounded, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer cancel()
			result, err := next(bounded, method, req)
			if err != nil {
				h.remove(session, "")
				go session.Close()
			}
			return result, err
		}
		return next(ctx, method, req)
	}
}
func (s *Server) mcpContext(ctx context.Context, header http.Header) (context.Context, error) {
	r := &http.Request{Header: header}
	r = r.WithContext(ctx)
	if !s.bearer(r) {
		return ctx, workspace.ErrForbidden
	}
	account, err := s.catalog.Account(subject(r.Context()))
	if err != nil {
		return ctx, err
	}
	client, _ := r.Context().Value(clientKey{}).(string)
	ctx = attribution.WithSource(r.Context(), account.Identity(), "mcp", client)
	if lease, ok := r.Context().Value(leaseKey{}).(revisionLease); ok {
		return context.WithValue(ctx, leaseKey{}, lease), nil
	}
	return context.WithValue(ctx, leaseKey{}, revisionLease{subject(ctx), digest(strings.TrimPrefix(header.Get("Authorization"), "Bearer "))}), nil
}
func (h *revisionHub) receiving(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		// SDK control notifications can be dispatched after their POST has
		// acknowledged enqueueing with 202. They have no action/output lifetime;
		// their params and bounded queue remain owned by the SDK.
		control := method == "notifications/initialized" || method == "notifications/cancelled" || method == "notifications/roots/list_changed" || method == "notifications/progress"
		// Never inherit the initialization POST's transport lifetime.
		ctx = context.WithValue(ctx, rpcHTTPKey{}, (*rpcHTTPLifetime)(nil))
		ctx = context.WithValue(ctx, rpcMethodKey{}, (*rpcMethodWork)(nil))
		if extra := req.GetExtra(); extra != nil {
			var err error
			ctx, err = h.app.mcpContext(ctx, extra.Header)
			if err != nil {
				return nil, err
			}
			lifetime := h.app.rpcHTTP.lookup(extra.Header)
			if lifetime == nil && !control {
				return nil, context.Canceled
			}
			ctx = context.WithValue(ctx, rpcHTTPKey{}, lifetime)
			// The SDK has already cancelled the named request if it runs here; a
			// legacy session's request may run on any gateway. Cancellations a
			// gateway hears carry no credential and stop above, unannounced.
			if cancelled, ok := req.GetParams().(*mcp.CancelledParams); ok && cancelled != nil {
				if id := legacyMCPSession(extra.Header); id != "" {
					h.app.legacy.announceCancel(id, cancelled.RequestID)
				}
			}
		}
		if method == "tools/call" {
			work := &rpcMethodWork{}
			ctx = context.WithValue(ctx, rpcMethodKey{}, work)
			defer work.finish()
			result, err := next(ctx, method, req)
			lifetime, _ := ctx.Value(rpcHTTPKey{}).(*rpcHTTPLifetime)
			// Avoid framing a large discarded result after an HTTP disconnect.
			// The SDK owns the finite envelope-encoding tail after this boundary.
			if lifetime.isClosed() {
				return nil, context.Canceled
			}
			if err == nil {
				err = h.app.checkActionCredential(ctx, req.(*mcp.CallToolRequest).Params.Name)
			}
			if err != nil {
				return nil, err
			}
			return result, nil
		}
		if method != "tools/call" && !control {
			admission := h.app.rpcWork
			switch method {
			case "initialize", "server/discover", "ping", "tools/list", "resources/list", "resources/read", "resources/templates/list", "prompts/list", "prompts/get":
				admission = h.app.rpcReadWork
			case "subscriptions/listen":
				admission = h.app.rpcStreams
				// A stopping server ends the stream at once; its client
				// listens again, reaching the next process.
				var stopListening context.CancelFunc
				ctx, stopListening = webcore.UntilDrain(ctx)
				defer stopListening()
			}
			release, err := admitRPC(ctx, admission)
			if err != nil {
				return nil, err
			}
			defer release()
		}
		if method == "resources/list" {
			resources := []*mcp.Resource{}
			if !h.app.hasScope(ctx, "notes:read") {
				return nil, workspace.ErrForbidden
			}
			for _, p := range h.app.clientProjects(ctx, subject(ctx)) {
				resources = append(resources, &mcp.Resource{URI: revisionURI(p.ID), Name: p.Name + " revision", MIMEType: "application/json"})
			}
			return &mcp.ListResourcesResult{Resources: resources}, nil
		}
		if method == "tools/list" {
			result, err := next(ctx, method, req)
			if err != nil {
				return result, err
			}
			if listed, ok := result.(*mcp.ListToolsResult); ok {
				out := *listed
				out.Tools = []*mcp.Tool{}
				for _, tool := range listed.Tools {
					if a, exists := h.app.actions[tool.Name]; exists && h.app.clientActionAvailable(ctx, tool.Name, a.ClientScope) {
						out.Tools = append(out.Tools, mcpListedTool(tool))
					}
				}
				return &out, nil
			}
			return result, nil
		}
		return next(ctx, method, req)
	}
}
func (h *revisionHub) read(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	store, err := h.app.toolStore(ctx, revisionProject(req.Params.URI), "read")
	if err != nil {
		return nil, workspace.ErrForbidden
	}
	status, err := store.Status(ctx)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(map[string]string{"revision": status.Revision})
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(raw)}}}, nil
}
func (s *Server) subscriptionTransport(server *mcp.Server) http.Handler {
	opts := &mcp.StreamableHTTPOptions{JSONResponse: true, SessionTimeout: mcpSessionIdle, MaxRequestBodyBytes: encodedUploadRequestLimit(), DisableLocalhostProtection: true}
	s.legacy = &legacySessions{app: s, server: server, handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, opts), local: map[string]localSession{}}
	s.legacy.listen()
	modern := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: encodedUploadRequestLimit(), DisableLocalhostProtection: true})
	secured := auth.RequireBearerToken(func(ctx context.Context, token string, r *http.Request) (*auth.TokenInfo, error) {
		if !s.bearer(r) {
			return nil, auth.ErrInvalidToken
		}
		// OAuth sessions follow consent; credentials retain their own identity.
		key := digest(token)
		expiry := time.Now().Add(time.Hour)
		if !s.adminBearer(r) {
			access, oauth, credential, api := s.oauth.BearerSnapshot(key)
			if api {
				expiry = time.Unix(credential.Expires, 0)
			} else if oauth {
				key = "grant:" + access.Family
				expiry = time.Unix(access.Expires, 0)
			}
		}
		return &auth.TokenInfo{UserID: key, Expiration: expiry}, nil
	}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NewResponseController(w).SetWriteDeadline(time.Time{})
		r.Header.Del(mcpSessionKey)
		if r.Header.Get("MCP-Protocol-Version") >= "2026-07-28" {
			modern.ServeHTTP(&mcpStreamWriter{w}, r)
		} else {
			s.legacy.serve(&mcpStreamWriter{w}, r)
		}
	}))
	return secured
}

type mcpStreamWriter struct{ http.ResponseWriter }

func (w *mcpStreamWriter) Write(p []byte) (int, error) {
	controller := http.NewResponseController(w.ResponseWriter)
	controller.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	defer controller.SetWriteDeadline(time.Time{})
	return w.ResponseWriter.Write(p)
}

func (w *mcpStreamWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *mcpStreamWriter) Flush() {
	c := http.NewResponseController(w.ResponseWriter)
	c.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	defer c.SetWriteDeadline(time.Time{})
	c.Flush()
}
