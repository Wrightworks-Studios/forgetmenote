// Package sourcehttp owns source import HTTP requests and previews.
// Durable jobs remain in sources; notebook authority remains in the
// instance's shared access checks.
package sourcehttp

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
	"net/http"
	"strconv"
	"time"
)

// Hooks bind the existing authorization and credential-restricted job scope.
// They are required; an omitted guard cannot enable a second path. The signer
// retains its concrete instance owner.
type Hooks struct {
	Scope  func(context.Context, string) sources.JobScope
	Access func(context.Context, string, string) (*workspace.Store, error)
	Sign   func(string) string
}
type HTTP struct {
	jobs     *sources.Jobs
	renderer *webcore.Renderer
	hooks    Hooks
	work     chan struct{}
}

func New(jobs *sources.Jobs, renderer *webcore.Renderer, work chan struct{}, hooks Hooks) (*HTTP, error) {
	if jobs == nil || renderer == nil || cap(work) == 0 || hooks.Scope == nil || hooks.Access == nil || hooks.Sign == nil {
		return nil, errors.New("source HTTP requires shared jobs, rendering and authority")
	}
	return &HTTP{jobs: jobs, renderer: renderer, hooks: hooks, work: work}, nil
}
func (h *HTTP) Register(register func(string, string, http.HandlerFunc)) {
	register("POST /api/sources/preview", "edit", h.Preview)
	register("GET /api/sources/verify", "read", h.Verify)
	register("GET /api/sources/formats", "read", h.FormatsHTTP)
	register("GET /api/sources/awaiting-text", "read", h.AwaitingTextHTTP)
	register("POST /api/sources/import", "edit", h.Import)
	register("POST /api/sources/jobs", "edit", h.StartJob)
	register("GET /api/sources/jobs/{job}", "edit", h.GetJob)
	register("POST /api/sources/jobs/{job}/cancel", "edit", h.CancelJob)
	register("POST /api/sources/jobs/{job}/resume", "edit", h.ResumeJob)
}
func (h *HTTP) Admit() (func(), error) {
	select {
	case h.work <- struct{}{}:
		return func() { <-h.work }, nil
	default:
		return nil, webcore.Error{Status: 429, Code: "capacity", Message: "Source import capacity reached; retry shortly"}
	}
}

type Input struct {
	Folder   *string `json:"folder,omitempty" jsonschema:"Destination folder for a new source note. Omit for sources; empty string selects project root. Do not pass with path when refreshing."`
	Project  string  `json:"project"`
	URL      string  `json:"url,omitempty" jsonschema:"Public source URL. With data, records provenance only; without data, fetches the URL."`
	Filename string  `json:"filename,omitempty"`
	Data     string  `json:"data,omitempty" jsonschema:"Base64 supported source original (see source_formats), bounded by operator FMN_LIMIT_SOURCE_BYTES when decoded. Omit to fetch URL."`
	Title    string  `json:"title,omitempty"`
	Path     string  `json:"path,omitempty" jsonschema:"Existing archived source note to refresh; omit for a new source."`
	Revision string  `json:"revision,omitempty" jsonschema:"Required current note revision when refreshing an existing source."`
	// An agent's own extraction replaces the server's. Only the synchronous
	// archive and preview take it; durable jobs exist for server extraction.
	Text      *sources.SuppliedText `json:"text,omitempty" jsonschema:"Your own extraction of the original in data, for a format source_formats marks extractedBy agent (or a PDF with scanned pages). The server stores it instead of extracting, labelled client-supplied and unverified. Requires data and extractor."`
	Extractor string                `json:"extractor,omitempty" jsonschema:"With text: the agent and model that extracted it, such as 'Example Agent 2; example-model-5'. One line of at most 200 characters."`
}
type Prepared struct {
	Capture   workspace.SourceCapture `json:"capture"`
	Expires   int64                   `json:"expires"`
	Signature string                  `json:"signature"`
	Preview   string                  `json:"preview"`
	HTML      string                  `json:"html,omitempty"`
}

type FormatView struct {
	workspace.SourceFormat
	// ExtractedBy is "server" or "agent": whether a connected agent should
	// read the original itself and upload its text with it.
	ExtractedBy   string `json:"extractedBy"`
	AgentExtracts string `json:"agentExtracts,omitempty"`
}

type JobOutput struct {
	Job      sources.JobStatus `json:"job"`
	Prepared *Prepared         `json:"prepared,omitempty"`
}

func decodeSource(in Input) ([]byte, error) {
	if len(in.Data) > base64.StdEncoding.EncodedLen(workspace.MaxSourceBytes()) {
		return nil, webcore.Error{Status: 400, Code: "invalid_request", Message: "source exceeds FMN_LIMIT_SOURCE_BYTES"}
	}
	var data []byte
	if in.Data != "" {
		var err error
		data, err = base64.StdEncoding.Strict().DecodeString(in.Data)
		if err != nil || len(data) == 0 {
			return nil, webcore.Error{Status: 400, Code: "invalid_request", Message: "provide a nonempty base64 file"}
		}
	}
	return data, nil
}

func (h *HTTP) signature(ctx context.Context, p Prepared) string {
	raw, _ := json.Marshal(p.Capture)
	project := webcore.NotebookID(ctx)
	return h.hooks.Sign("source-preview\x00" + auth.Subject(ctx) + "\x00" + project + "\x00" + strconv.FormatInt(p.Expires, 10) + "\x00" + string(raw))
}
func (h *HTTP) Preview(w http.ResponseWriter, r *http.Request) {
	release, err := h.Admit()
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), sources.ImportTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	var in Input
	if !webcore.Decode(w, r, &in) {
		return
	}
	capture, err := h.Prepare(r.Context(), webcore.NotebookID(r.Context()), in)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, h.Prepared(r.Context(), capture))
}
func (h *HTTP) Prepared(ctx context.Context, capture workspace.SourceCapture) Prepared {
	out := Prepared{Capture: capture, Expires: time.Now().Add(15 * time.Minute).Unix(), Preview: capture.Text}
	if runes := []rune(out.Preview); len(runes) > 8000 {
		out.Preview = string(runes[:8000]) + "\n\nPreview shortened; the full extracted text will be archived."
	}
	out.HTML = h.renderer.Render(out.Preview)
	out.Signature = h.signature(ctx, out)
	return out
}
func (h *HTTP) Import(w http.ResponseWriter, r *http.Request) {
	release, err := h.Admit()
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	defer release()
	var in struct {
		Job      string   `json:"job,omitempty"`
		Folder   *string  `json:"folder,omitempty"`
		Prepared Prepared `json:"prepared"`
		Title    string   `json:"title"`
		Path     string   `json:"path"`
		Revision string   `json:"revision"`
	}
	if !webcore.Decode(w, r, &in) {
		return
	}
	capture := in.Prepared.Capture
	if in.Job != "" {
		var err error
		capture, err = h.jobs.Result(r.Context(), h.hooks.Scope(r.Context(), webcore.NotebookID(r.Context())), in.Job)
		if err != nil {
			webcore.Fail(w, err)
			return
		}
	} else if in.Prepared.Expires < time.Now().Unix() || !hmac.Equal([]byte(in.Prepared.Signature), []byte(h.signature(r.Context(), in.Prepared))) {
		http.Error(w, "Source preview expired or changed; preview it again", 403)
		return
	}
	if _, err := h.hooks.Access(r.Context(), webcore.NotebookID(r.Context()), "edit"); err != nil {
		webcore.Fail(w, err)
		return
	}
	doc, err := webcore.NotebookStore(r.Context()).ImportSource(r.Context(), capture, in.Path, in.Revision, in.Title, in.Folder)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, h.renderer.Output(doc))
}
func (h *HTTP) FormatsHTTP(w http.ResponseWriter, r *http.Request) {
	webcore.JSON(w, Formats())
}

func (h *HTTP) Verify(w http.ResponseWriter, r *http.Request) {
	result, err := webcore.NotebookStore(r.Context()).VerifySource(r.Context(), r.URL.Query().Get("path"), r.URL.Query().Get("revision"))
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, result)
}

func Formats() []FormatView {
	var out []FormatView
	for _, format := range sources.Formats() {
		by, agentPart := sources.ExtractedBy(format)
		out = append(out, FormatView{SourceFormat: format, ExtractedBy: by, AgentExtracts: agentPart})
	}
	return out
}

// AwaitingSource is an archived source note without usable text, with what
// an agent needs to refresh it: the path, the revision it was listed at and
// the original to read.
type AwaitingSource struct {
	Path       string `json:"path"`
	Title      string `json:"title"`
	Format     string `json:"format"`
	MIME       string `json:"mime"`
	Extraction string `json:"extraction"`
	Warning    string `json:"warning,omitempty"`
	Original   string `json:"original"`
	Revision   string `json:"revision"`
}

type AwaitingText struct {
	Revision string           `json:"revision"`
	Sources  []AwaitingSource `json:"sources"`
	// More says the listing stopped at its limit; sources given text leave it.
	More bool `json:"more,omitempty"`
}

const awaitingLimit = 200

// Awaiting lists the notebook's archived sources that wait for an agent's
// text, at one revision: originals stored without reading, and extractions
// that failed, were partial or found no text.
func Awaiting(ctx context.Context, store *workspace.Store) (AwaitingText, error) {
	revision, entries, err := store.ArchivedSources(ctx)
	if err != nil {
		return AwaitingText{}, err
	}
	out := AwaitingText{Revision: revision, Sources: []AwaitingSource{}}
	for _, entry := range entries {
		record := entry.Metadata.Source
		if !sources.AwaitingText(*record) {
			continue
		}
		if len(out.Sources) == awaitingLimit {
			out.More = true
			break
		}
		out.Sources = append(out.Sources, AwaitingSource{Path: entry.Path, Title: entry.Metadata.Title, Format: workspace.SourceFormatForMIME(record.MIME).Name, MIME: record.MIME, Extraction: record.Extraction, Warning: record.Warning, Original: record.Original, Revision: revision})
	}
	return out, nil
}

func (h *HTTP) AwaitingTextHTTP(w http.ResponseWriter, r *http.Request) {
	out, err := Awaiting(r.Context(), webcore.NotebookStore(r.Context()))
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, out)
}

func (h *HTTP) Submit(ctx context.Context, project string, in Input) (sources.JobStatus, error) {
	if in.Text != nil || in.Extractor != "" {
		return sources.JobStatus{}, webcore.Error{Status: 400, Code: "invalid_request", Message: "source jobs run server extraction; send your own text with archive_source or the source preview"}
	}
	data, err := decodeSource(in)
	if err != nil {
		return sources.JobStatus{}, err
	}
	input := sources.JobInput{Filename: in.Filename, URL: in.URL, Path: in.Path, Folder: in.Folder}
	return h.jobs.Submit(ctx, h.hooks.Scope(ctx, project), input, data)
}
func (h *HTTP) Prepare(ctx context.Context, project string, in Input) (workspace.SourceCapture, error) {
	if in.Text != nil || in.Extractor != "" {
		return h.prepareSupplied(ctx, project, in)
	}
	job, err := h.Submit(ctx, project, in)
	if err != nil {
		return workspace.SourceCapture{}, err
	}
	scope := h.hooks.Scope(ctx, project)
	capture, err := h.jobs.Wait(ctx, scope, job.ID)
	if ctx.Err() != nil {
		_, _ = h.jobs.Cancel(context.WithoutCancel(ctx), scope, job.ID)
	}
	return capture, err
}

// prepareSupplied makes the capture of an original and its uploader's text
// under a source job's edit check, without its extraction.
func (h *HTTP) prepareSupplied(ctx context.Context, project string, in Input) (workspace.SourceCapture, error) {
	if in.Text == nil {
		return workspace.SourceCapture{}, webcore.Error{Status: 400, Code: "invalid_request", Message: "extractor labels supplied text; send text with it, or neither"}
	}
	if len(in.Filename) > 1024 || len(in.Path) > 512 {
		return workspace.SourceCapture{}, webcore.Error{Status: 400, Code: "invalid_request", Message: "source filename or path exceeds its budget"}
	}
	data, err := decodeSource(in)
	if err != nil {
		return workspace.SourceCapture{}, err
	}
	if _, err := h.hooks.Access(ctx, project, "edit"); err != nil {
		return workspace.SourceCapture{}, err
	}
	return sources.PrepareSupplied(in.Filename, in.URL, data, in.Extractor, *in.Text)
}

func (h *HTTP) JobOutput(ctx context.Context, scope sources.JobScope, id string) (JobOutput, error) {
	job, err := h.jobs.Status(ctx, scope, id)
	if err != nil {
		return JobOutput{}, err
	}
	out := JobOutput{Job: job}
	if job.State == "ready" || job.State == "partial" {
		capture, err := h.jobs.Result(ctx, scope, id)
		if err != nil {
			return out, err
		}
		// The server retains the original. Async clients save by job ID and do not
		// shuttle the upload through preview JSON a second time.
		capture.Original = nil
		prepared := h.Prepared(ctx, capture)
		out.Prepared = &prepared
	}
	return out, nil
}
func (h *HTTP) StartJob(w http.ResponseWriter, r *http.Request) {
	release, err := h.Admit()
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	defer release()
	var in Input
	if !webcore.Decode(w, r, &in) {
		return
	}
	job, err := h.Submit(r.Context(), webcore.NotebookID(r.Context()), in)
	if errors.Is(err, sources.ErrJobCapacity) {
		http.Error(w, err.Error(), 429)
		return
	}
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, JobOutput{Job: job})
}
func (h *HTTP) GetJob(w http.ResponseWriter, r *http.Request) {
	out, err := h.JobOutput(r.Context(), h.hooks.Scope(r.Context(), webcore.NotebookID(r.Context())), r.PathValue("job"))
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, out)
}
func (h *HTTP) CancelJob(w http.ResponseWriter, r *http.Request) {
	r.SetPathValue("action", "cancel")
	h.controlJob(w, r)
}
func (h *HTTP) ResumeJob(w http.ResponseWriter, r *http.Request) {
	r.SetPathValue("action", "resume")
	h.controlJob(w, r)
}
func (h *HTTP) controlJob(w http.ResponseWriter, r *http.Request) {
	scope := h.hooks.Scope(r.Context(), webcore.NotebookID(r.Context()))
	id := r.PathValue("job")
	var job sources.JobStatus
	var err error
	switch r.PathValue("action") {
	case "cancel":
		job, err = h.jobs.Cancel(r.Context(), scope, id)
	case "resume":
		job, err = h.jobs.Resume(r.Context(), scope, id)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, JobOutput{Job: job})
}
