package chatrun

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"forgetmenote/internal/auth"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

// Register adds the chat routes. Each conversation route goes through
// register, which admits only a reader of its notebook; the connections a
// person may choose are listed through global, outside any notebook.
func (s *Service) Register(register func(pattern, role string, handler http.HandlerFunc), global func(pattern string, handler func(http.ResponseWriter, *http.Request))) {
	global("GET /api/chat/connections", s.chatConnections)
	register("GET /api/chat", "read", s.chatList)
	register("POST /api/chat", "read", s.chatCreate)
	register("POST /api/chat/{chat}/turns", "read", s.chatTurn)
	register("POST /api/chat/{chat}/runs/{run}/cancel", "read", s.chatCancel)
	register("POST /api/chat/{chat}/runs/{run}/resume", "read", s.chatResume)
	register("POST /api/chat/{chat}/save-note", "read", s.chatSaveNote)
	register("POST /api/chat/{chat}/actions/{action}", "read", s.chatAction)
	register("GET /api/chat/{chat}", "read", s.chatRead)
	register("DELETE /api/chat/{chat}", "read", s.chatDelete)
	register("GET /api/chat/{chat}/events", "read", s.chatEvents)
	register("GET /api/chat/{chat}/runs/{run}/citations", "read", s.judgmentCitations)
	register("POST /api/chat/{chat}/runs/{run}/citations", "read", s.judgmentCitations)
}

func (s *Service) chatAuthorized(r *http.Request) (chat.Conversation, error) {
	user, err := s.hooks.Interactive(r)
	if err != nil {
		return chat.Conversation{}, err
	}
	project := webcore.NotebookID(r.Context())
	if _, err = s.catalog.Access(user, project, "read"); err != nil {
		return chat.Conversation{}, err
	}
	c, err := s.hooks.Chat().Store.Read(user, project, r.PathValue("chat"))
	if err == nil {
		if err = s.hooks.CheckDataConversation(r.Context(), c); err != nil {
			return chat.Conversation{}, err
		}
		if err = s.checkConversationNotebooks(user, c); err != nil {
			return chat.Conversation{}, err
		}
	}
	return c, err
}

// checkConversationNotebooks requires current read access to every notebook a
// conversation drew on: each run's note and source corpus and every cited
// notebook or source. The transcript keeps their passages, so losing any one
// withholds the whole conversation, as a revoked data grant does. Deletion
// does not read the transcript and stays available.
func (s *Service) checkConversationNotebooks(user string, c chat.Conversation) error {
	checked := map[string]bool{"": true}
	check := func(project string) error {
		if checked[project] {
			return nil
		}
		checked[project] = true
		if _, err := s.catalog.Access(user, project, "read"); err != nil {
			return workspace.ErrForbidden
		}
		return nil
	}
	corpus := func(k *chat.Corpus) error {
		if k == nil {
			return nil
		}
		for _, project := range append(append([]string(nil), k.NoteProjects...), k.SourceProjects...) {
			if err := check(project); err != nil {
				return err
			}
		}
		return nil
	}
	cited := func(citations []chat.Citation) error {
		for _, citation := range citations {
			if err := check(citation.Project); err != nil {
				return err
			}
		}
		return nil
	}
	if err := corpus(c.Options.Corpus); err != nil {
		return err
	}
	for _, run := range c.Runs {
		if err := corpus(run.Options.Corpus); err != nil {
			return err
		}
		if err := cited(run.Citations); err != nil {
			return err
		}
		for _, op := range run.Operations {
			if err := cited(op.Citations); err != nil {
				return err
			}
		}
	}
	for _, turn := range c.Turns {
		if err := cited(turn.Citations); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) chatConnections(w http.ResponseWriter, r *http.Request) {
	user, err := s.hooks.Interactive(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	cfg := s.hooks.Chat().Snapshot().Config
	var defaultID string
	var localOnly, research bool
	if cfg != nil {
		defaultID = cfg.DefaultConnection
		localOnly = cfg.LocalOnly
		research = cfg.Research != nil && cfg.Research.Enabled && !cfg.LocalOnly
	}
	usage, err := s.hooks.Chat().Store.PersonalUsage(user)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, map[string]any{"usage": usage, "enabled": cfg != nil, "connections": cfg.PublicConnections(), "defaultConnection": defaultID, "localOnly": localOnly, "research": research, "actions": s.hooks.ActionCatalog()})
}

func (s *Service) chatList(w http.ResponseWriter, r *http.Request) {
	content, release := s.hooks.Chat().RetainContent(auth.Subject(r.Context()), "")
	defer release()
	content.RLock()
	defer content.RUnlock()
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	user, err := s.hooks.Interactive(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	all, err := s.hooks.Chat().Store.List(user, webcore.NotebookID(r.Context()))
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	items := []map[string]any{}
	for _, c := range all {
		state := ""
		if len(c.Runs) > 0 {
			state = c.Runs[len(c.Runs)-1].State
		}
		items = append(items, map[string]any{"id": c.ID, "title": c.Title, "project": c.Project, "updatedAt": c.UpdatedAt, "options": c.Options, "state": state, "usage": c.Usage(), "error": c.Error, "unavailable": c.Unavailable})
	}
	webcore.JSON(w, map[string]any{"conversations": items})
}

func (s *Service) chatCreate(w http.ResponseWriter, r *http.Request) {
	user, err := s.hooks.Interactive(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	var opts chat.Options
	if !webcore.Decode(w, r, &opts) {
		return
	}
	if opts.Mode == "" {
		opts.Mode = "ask"
	}
	if err = validateChatOptions(opts); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	cfg := s.hooks.Chat().Snapshot().Config
	connection, err := cfg.Resolve(opts.Connection, opts.Model)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	opts.Connection = connection.ID
	if opts.Model == "" {
		opts.Model = connection.DefaultModel
	}
	opts.Corpus, err = s.hooks.ResolveCorpus(r.Context(), webcore.NotebookStore(r.Context()), opts.Corpus, nil, false)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	c, err := s.hooks.Chat().Store.Create(user, webcore.NotebookID(r.Context()), opts)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, c)
}

func validateChatOptions(opts chat.Options) error {
	if opts.Mode != "ask" && opts.Mode != "suggest" && opts.Mode != "edit" {
		return errors.New("Choose Ask, Suggest or Edit")
	}
	if len(opts.Actions) > 4 {
		return errors.New("Too many action scopes")
	}
	for _, action := range opts.Actions {
		if !slices.Contains([]string{"notes", "live", "comments", "review"}, action) {
			return errors.New("Unknown action scope")
		}
	}
	return nil
}

// chatTranscript reconstructs unfinished streamed turns at their original event
// position. Reads, paging and explicit exports share this durable view; provider
// continuation still uses finalized messages only.
func chatTranscript(c chat.Conversation) []chat.Turn {
	final := map[string]bool{}
	for _, turn := range c.Turns {
		final[turn.ID] = true
	}
	firstEvent := map[string]int{}
	ids := make([]string, len(c.Events))
	for i, event := range c.Events {
		var item struct {
			ID     string `json:"id"`
			TurnID string `json:"turnId"`
		}
		if event.Type != "user" && event.Type != "assistant" && event.Type != "text_delta" {
			continue
		}
		if json.Unmarshal(event.Data, &item) != nil {
			continue
		}
		id := item.ID
		if event.Type == "text_delta" {
			id = item.TurnID
		}
		if id == "" {
			continue
		}
		ids[i] = id
		if _, exists := firstEvent[id]; !exists {
			firstEvent[id] = i
		}
	}
	out := []chat.Turn{}
	partial := map[string]int{}
	citations := map[string][]chat.Citation{}
	next := 0
	for i, event := range c.Events {
		if event.Type == "citations" {
			var sources []chat.Citation
			if json.Unmarshal(event.Data, &sources) == nil {
				citations[event.Run] = sources
			}
		}
		id := ids[i]
		if id == "" {
			continue
		}
		for next < len(c.Turns) {
			at, exists := firstEvent[c.Turns[next].ID]
			if exists && at > i {
				break
			}
			out = append(out, c.Turns[next])
			next++
		}
		if event.Type != "text_delta" || final[id] {
			continue
		}
		var delta struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(event.Data, &delta) != nil {
			continue
		}
		index, exists := partial[id]
		if !exists {
			sources, haveSources := citations[event.Run]
			active := false
			if run := c.Run(event.Run); run != nil {
				active = chat.Active(run.State)
				if !haveSources {
					sources = run.Citations
				}
			}
			index = len(out)
			partial[id] = index
			out = append(out, chat.Turn{ID: id, RunID: event.Run, Role: "assistant", Streaming: active, Citations: append([]chat.Citation(nil), sources...)})
		}
		out[index].Content += delta.Text
	}
	out = append(out, c.Turns[next:]...)
	return out
}

func (s *Service) chatRead(w http.ResponseWriter, r *http.Request) {
	content, release := s.hooks.Chat().RetainContent(auth.Subject(r.Context()), r.PathValue("chat"))
	defer release()
	content.RLock()
	defer content.RUnlock()
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	c, err := s.chatAuthorized(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	turns := chatTranscript(c)
	before := len(turns)
	if v := r.URL.Query().Get("before"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 0 || n > before {
			http.Error(w, "Invalid transcript cursor", 400)
			return
		}
		before = n
	}
	start := max(0, before-100)
	total := len(turns)
	cursor := len(c.Events)
	c.Turns = turns[start:before]
	for i := range c.Turns {
		c.Turns[i].Intermediate = c.Turns[i].Role == "assistant" && c.Turns[i].Provider != nil && len(c.Turns[i].Provider.ToolCalls) > 0
		c.Turns[i].Provider = nil
	}
	for i := range c.Runs {
		// The gateway holding a run only guides recovery between gateways.
		c.Runs[i].Messages, c.Runs[i].Holder = nil, ""
	}
	c.Events = nil
	webcore.JSON(w, struct {
		chat.Conversation
		EventCursor int `json:"eventCursor"`
		TotalTurns  int `json:"totalTurns"`
		NextBefore  int `json:"nextBefore"`
	}{c, cursor, total, start})
}

func (s *Service) chatEvents(w http.ResponseWriter, r *http.Request) {
	c, err := s.chatAuthorized(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	raw := r.URL.Query().Get("after")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	if raw == "" {
		raw = "0"
	}
	after, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || after < 0 || after > int64(len(c.Events)) {
		http.Error(w, "Invalid chat event cursor", 400)
		return
	}
	releaseStream, err := s.hooks.Chat().RetainStream(c.Owner)
	if err != nil {
		http.Error(w, "Too many chat streams; close another tab and reconnect", 429)
		return
	}
	defer releaseStream()
	content, release := s.hooks.Chat().RetainContent(c.Owner, c.ID)
	defer release()
	var generation uint64
	c, generation, err = s.hooks.Chat().Store.ReadVersion(c.Owner, c.Project, c.ID)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	controller := http.NewResponseController(w)
	if controller.SetWriteDeadline(time.Time{}) != nil {
		http.Error(w, "Streaming unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-store")
	epoch := s.catalog.Epoch(c.Owner)
	const poll = 250 * time.Millisecond
	tick := time.NewTicker(poll)
	defer tick.Stop()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	// A stopping server ends the stream at once; the page's EventSource
	// reconnects, reaching the next process.
	streaming, stopStreaming := webcore.UntilDrain(r.Context())
	defer stopStreaming()
	// The transcript-wide grant checks cost O(runs and citations). Run them
	// for each newly read conversation before its first event, and again
	// every poll while its events are sent, never once per event.
	authorize := func() error {
		if e := s.hooks.CheckDataConversation(r.Context(), c); e != nil {
			return e
		}
		if e := s.checkConversationNotebooks(c.Owner, c); e != nil {
			return e
		}
		if !s.hooks.Authenticated(r) || auth.Subject(r.Context()) != c.Owner {
			return workspace.ErrForbidden
		}
		if !s.catalog.ValidSession(c.Owner, epoch) {
			return workspace.ErrForbidden
		}
		_, e := s.catalog.Access(c.Owner, c.Project, "read")
		return e
	}
	send := func(event chat.EventRecord) error {
		content.RLock()
		defer content.RUnlock()
		if content.Deleted {
			return chat.ErrNotFound
		}
		if e := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); e != nil {
			return e
		}
		b, e := json.Marshal(event)
		if e != nil {
			return e
		}
		if _, e = w.Write([]byte("id: " + strconv.FormatInt(event.Sequence, 10) + "\nevent: chat\ndata: " + string(b) + "\n\n")); e != nil {
			return e
		}
		return controller.Flush()
	}
	for {
		var authorized time.Time
		for _, e := range c.Events {
			if e.Sequence <= after {
				continue
			}
			if time.Since(authorized) >= poll {
				if authorize() != nil {
					return
				}
				authorized = time.Now()
			}
			if send(e) != nil {
				return
			}
			after = e.Sequence
		}
		select {
		case <-streaming.Done():
			return
		case <-heartbeat.C:
			if !s.hooks.Authenticated(r) {
				return
			}
			if _, e := s.catalog.Access(c.Owner, c.Project, "read"); e != nil {
				return
			}
			controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, e := w.Write([]byte(": keepalive\n\n")); e != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
		case <-tick.C:
			closed := s.hooks.Chat().Snapshot().Closed
			if closed {
				return
			}
			if !s.hooks.Authenticated(r) || !s.catalog.ValidSession(c.Owner, epoch) {
				return
			}
			if _, e := s.catalog.Access(c.Owner, c.Project, "read"); e != nil {
				return
			}
			content.RLock()
			retired := content.Deleted
			content.RUnlock()
			if retired {
				return
			}
			if s.hooks.Chat().Store.Generation(c.Owner, c.ID) == generation {
				continue
			}
			c, generation, err = s.hooks.Chat().Store.ReadVersion(c.Owner, c.Project, c.ID)
			if err != nil {
				return
			}
		}
	}
}
