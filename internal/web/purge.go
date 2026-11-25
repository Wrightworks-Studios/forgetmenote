package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/durable"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

// Purge (storage v2 design §11) permanently erases content from every
// version of a notebook: a whole note, chosen versions of one, or text
// redacted from selected notes or the whole notebook. Only a notebook Owner
// or an instance Admin purges, in the browser (a signed-in session; API and
// MCP credentials never reach these routes) or through the operator CLI with
// the instance access key. An MCP agent only prepares a plan for a person to
// confirm in the browser. Preparing changes nothing; executing runs exactly
// the confirmed plan, then cleans what the instance derived from the purged
// content and keeps a receipt that names who, when, why and what kind of
// purge, never the content.

// purgeExposure is what every plan and receipt says purge cannot do.
const purgeExposure = "A purge cannot undo exposure: rotate any leaked secret regardless; GitHub may keep cached views of old commits until GitHub Support clears them; downloads, earlier backups and pg_dump files taken before the purge still hold the content."

// purgePreviousRelease is what a plan and a receipt add while the previous
// release's files for the notebook are still in the data directory
// (previousReleaseFiles).
const purgePreviousRelease = "The previous release's files for this notebook (its Git working tree and repository, live edits and captures) are still in the data directory and hold its content from before the move to the database; no purge reaches them. Move them out of the live data directory into the offline backup taken then, and delete them; re-take or delete every complete backup that holds them."

// A purge plan made for an agent waits for a person this long.
const purgePlanTTL = time.Hour

// The largest purge scope: a text or pattern of 64 KiB, and 1,000 notes or
// versions.
const (
	maxPurgeText  = 64 << 10
	maxPurgePaths = 1000
)

// purgeWork bounds a confirmed purge, which runs to its end even when its
// request ends; the operator command waits as long for its answer.
const purgeWork = 30 * time.Minute

const purgeReceiptKind = "purge-receipt"

var errPurgedVersion = webcore.Error{Status: http.StatusGone, Code: "purged", Message: "This version was permanently erased by a purge."}

type purgePrepareInput struct {
	Scope workspace.PurgeScope `json:"scope"`
	// Token names a plan an agent prepared (prepare_purge) for this person
	// to review; it stands for the scope.
	Token string `json:"token,omitempty"`
}

type purgeExecuteInput struct {
	Scope        workspace.PurgeScope `json:"scope"`
	Token        string               `json:"token,omitempty"`
	Revision     string               `json:"revision"`
	Confirmation string               `json:"confirmation"`
	Reason       string               `json:"reason"`
	// Operator names the person behind the instance access key, which the
	// CLI uses, for the receipt.
	Operator string `json:"operator,omitempty"`
}

// purgeDerived counts what quoted the purged content outside the notebook,
// which a person's plan reports and a purge redacts without anyone reading
// it. Those records are matched only against what the notebook holds
// (PurgeQuotes), or, for an instance Admin in the browser, who may redact
// anyone's records, a text purge's text or pattern itself: a plan never tells
// anyone else whether other people's records hold what they guess. What the
// notebook holds is whatever its writers put there, so an agent's plan gets
// no counts at all: an agent that wrote guesses into a note could otherwise
// read other people's records by probing with patterns over them.
//
// Even so, the records are other people's, and what the notebook holds is
// what its writers chose: someone who is not an instance Admin is told how
// many of their own chats, proposals and Jev records quote the content
// (Own), and only that other people's are redacted too (Others), never how
// many, or a dictionary of guesses written into a note would tell them
// which their colleagues' private records hold.
type purgeDerived struct {
	Chats       int `json:"chats"`
	Proposals   int `json:"proposals"`
	JevRecords  int `json:"jevRecords"`
	Checkpoints int `json:"checkpoints,omitempty"`
	Vectors     int `json:"vectors,omitempty"`
	// Partial reports that some purged versions were too large to read in
	// full for what the purge redacts elsewhere.
	Partial bool `json:"partial,omitempty"`
	// Own reports counts of the caller's own records only; Others then says
	// that other people's are redacted too.
	Own    bool   `json:"own,omitempty"`
	Others string `json:"others,omitempty"`
	// Withheld reports a receipt that gives the viewer no counts: of a purge
	// someone else ran, which only they and instance Admins see, or of one
	// that stopped before it counted.
	Withheld bool `json:"withheld,omitempty"`
}

// What a plan or receipt that counts only the caller's own records says of
// everyone else's, and what a receipt of someone else's purge says.
const (
	purgeOthersRedacted = "Other people's private chat transcripts, Knowledge proposals and Jev records that quote it are redacted too; only an instance Admin is told how many."
	purgeOthersWithheld = "Private chat transcripts, Knowledge proposals and Jev records that quoted it were redacted; only the person who ran this purge and instance Admins are told how many."
)

// A purgePlanView is the workspace's plan with what the instance adds: the
// derived records that will be redacted, and what a purge cannot do.
type purgePlanView struct {
	workspace.PurgePlan
	// Derived is absent from an agent's plan (RecordsCountedOnReview).
	Derived *purgeDerived `json:"derived,omitempty"`
	// NotebookUnchanged reports a plan that finds the content only outside
	// the notebook; running it redacts those records.
	NotebookUnchanged bool   `json:"notebookUnchanged"`
	Exposure          string `json:"exposure"`
	// RecordsUnsearched reports a text purge of content the notebook does
	// not hold whose records outside the notebook were not searched: only an
	// instance Admin's plan, in the browser, searches them.
	RecordsUnsearched bool `json:"recordsUnsearched,omitempty"`
	// RecordsCountedOnReview reports a plan prepared for an agent, which
	// counts nothing outside the notebook: the records there that quote the
	// content are counted when a person opens the plan in the browser.
	RecordsCountedOnReview bool `json:"recordsCountedOnReview,omitempty"`
	// Warnings are what the person should know before running it that it
	// cannot change, such as copies the purge does not reach.
	Warnings []string `json:"warnings,omitempty"`
}

// A purgeReceipt is what a purge kept of itself: never the content, the
// paths it removed or the pattern.
type purgeReceipt struct {
	Purge         string `json:"purge"`
	Notebook      string `json:"notebook"`
	Actor         string `json:"actor"`
	ActorName     string `json:"actorName,omitempty"`
	Reason        string `json:"reason"`
	Created       string `json:"created"`
	Kind          string `json:"kind"`
	Pattern       bool   `json:"pattern,omitempty"`
	WholeNotebook bool   `json:"wholeNotebook,omitempty"`
	Notes         int    `json:"notes"`
	Versions      int    `json:"versions"`
	Messages      int    `json:"messages"`
	Revision      string `json:"revision"`
	Rewritten     int    `json:"rewritten"`
	// RemovedVersions and RemovedBlocks count what storage deleted. Content
	// another notebook keeps stays stored, so only an instance Admin, who
	// may read every notebook, sees them (purgeReceiptFor).
	RemovedVersions   int          `json:"removedVersions"`
	RemovedBlocks     int          `json:"removedBlocks,omitempty"`
	LiveReset         int          `json:"liveReset"`
	Export            bool         `json:"export"`
	ExportForcePush   bool         `json:"exportForcePush"`
	NotebookUnchanged bool         `json:"notebookUnchanged,omitempty"`
	Derived           purgeDerived `json:"derived"`
	// ActorDerived, kept with a purge someone who is not an instance Admin
	// ran, counts their own records, which their receipt shows them instead
	// of Derived (purgeReceiptFor).
	ActorDerived *purgeDerived `json:"actorDerived,omitempty"`
	Warnings     []string      `json:"warnings,omitempty"`
	Exposure     string        `json:"exposure"`
	// PreparedByAgent reports a plan an agent prepared (prepare_purge) that
	// a person reviewed and ran.
	PreparedByAgent bool `json:"preparedByAgent,omitempty"`
	// Unfinished reports a purge that rewrote the notebook and has kept no
	// receipt: it is still running, or the gateway running it stopped.
	// Interrupted is the receipt a starting gateway kept of a purge that
	// stopped, once it finished what it could (finishStoppedPurges). Their
	// warnings say what was not done and how to finish it.
	Unfinished  bool `json:"unfinished,omitempty"`
	Interrupted bool `json:"interrupted,omitempty"`
}

// purgeRunKey names the lock a purge of notebook holds, on whichever
// gateway runs it, from before it rewrites the history until it keeps its
// receipt: a recorded purge without a receipt whose lock is free stopped
// before it finished.
func purgeRunKey(notebook string) string { return "purge-run " + notebook }

// purgeTarget is the notebook a purge request names, with who asks.
type purgeTarget struct {
	project string
	store   *workspace.Store
	user    string
}

func (s *Server) purgeRoutes() {
	s.handleProject("POST /api/purge/prepare", "owner", func(w http.ResponseWriter, r *http.Request) {
		var in purgePrepareInput
		if !decode(w, r, &in) {
			return
		}
		target, err := s.purgeTarget(r.Context(), projectID(r.Context()), projectStore(r.Context()))
		if err == nil {
			in.Scope, err = s.purgeScope(r.Context(), target, in.Scope, in.Token)
		}
		var plan purgePlanView
		if err == nil {
			plan, err = s.preparePurge(r.Context(), target, in.Scope, false)
		}
		if err != nil {
			fail(w, purgeError(err))
			return
		}
		jsonResponse(w, plan)
	})
	s.handleProject("POST /api/purge/execute", "owner", func(w http.ResponseWriter, r *http.Request) {
		var in purgeExecuteInput
		if !decode(w, r, &in) {
			return
		}
		target, err := s.purgeTarget(r.Context(), projectID(r.Context()), projectStore(r.Context()))
		if err == nil {
			in.Scope, err = s.purgeScope(r.Context(), target, in.Scope, in.Token)
		}
		var receipt purgeReceipt
		if err == nil {
			receipt, err = s.executePurge(r.Context(), target, in)
		}
		if err != nil {
			fail(w, purgeError(err))
			return
		}
		jsonResponse(w, receipt)
	})
	s.handleProject("GET /api/purge", "owner", func(w http.ResponseWriter, r *http.Request) {
		receipts, err := s.purgeReceipts(r.Context(), projectID(r.Context()))
		for i := range receipts {
			receipts[i] = s.purgeReceiptFor(subject(r.Context()), receipts[i])
		}
		respond(w, map[string]any{"receipts": receipts}, err)
	})
}

// prepare_purge input: one purge scope, flattened for agents.
type preparePurgeInput struct {
	Project  string   `json:"project"`
	Kind     string   `json:"kind" jsonschema:"note (a whole note), versions (chosen versions of one note) or text (an exact string or pattern)"`
	Path     string   `json:"path,omitempty" jsonschema:"The note a note or versions purge removes"`
	Versions []string `json:"versions,omitempty" jsonschema:"For kind versions: the revisions that wrote the versions to remove, as note_history lists them"`
	Text     string   `json:"text,omitempty" jsonschema:"For kind text: the exact text to remove"`
	Pattern  string   `json:"pattern,omitempty" jsonschema:"For kind text: an RE2 regular expression whose every match is removed, instead of text"`
	Paths    []string `json:"paths,omitempty" jsonschema:"For kind text: the notes to redact; omit for the whole notebook"`
}

type preparePurgeOutput struct {
	Plan purgePlanView `json:"plan"`
	// ConfirmURL opens the plan in the browser, where a notebook Owner or an
	// Admin reviews it, gives a reason and runs it.
	ConfirmURL string `json:"confirmUrl"`
	Expires    string `json:"expires"`
	Message    string `json:"message"`
}

func (s *Server) purgeTools(server *mcp.Server) {
	addAction(s, server, &mcp.Tool{Name: "prepare_purge", Description: "Prepare a plan to permanently erase content from every version of a notebook: a whole note, chosen versions of one, or an exact text or pattern (such as a leaked API key) from chosen notes or the whole notebook. Requires Owner or Admin. Changes nothing: it returns the plan (affected notes and versions, other places the text appears, references remapped, whether the GitHub copy is force-pushed) and a browser URL where a person reviews it, gives a reason and runs it. Agents cannot run a purge. Tell the user that a purge cannot undo exposure: rotate leaked secrets regardless."}, func(ctx context.Context, in preparePurgeInput) (preparePurgeOutput, error) {
		store, err := s.toolStore(ctx, in.Project, "owner")
		if err != nil {
			return preparePurgeOutput{}, err
		}
		target, err := s.purgeTarget(ctx, in.Project, store)
		if err != nil {
			return preparePurgeOutput{}, err
		}
		scope := workspace.PurgeScope{Kind: in.Kind, Path: in.Path, Versions: in.Versions, Text: in.Text, Pattern: in.Pattern, Paths: in.Paths}
		plan, err := s.preparePurge(ctx, target, scope, true)
		if err != nil {
			return preparePurgeOutput{}, purgeError(err)
		}
		token := RandomToken()
		body, err := json.Marshal(struct {
			Project string               `json:"project"`
			Scope   workspace.PurgeScope `json:"scope"`
		}{in.Project, scope})
		if err != nil {
			return preparePurgeOutput{}, err
		}
		if err := s.db.BeginFlow(ctx, purgeFlowKind(in.Project), token, target.user, body, purgePlanTTL, pg.FlowBound{Limit: 64, Evict: true}); err != nil {
			return preparePurgeOutput{}, err
		}
		// The agent never holds what runs the plan.
		plan.Confirmation = ""
		plan.Scope.Text, plan.Scope.Pattern = "", ""
		link := s.origin + "/?" + url.Values{"project": {in.Project}, "purge": {token}}.Encode()
		return preparePurgeOutput{Plan: plan, ConfirmURL: link, Expires: time.Now().Add(purgePlanTTL).UTC().Format(time.RFC3339),
			Message: "Nothing was erased. Give the user this URL: a notebook Owner or Admin must review the plan in the browser, give a reason and confirm it there. Private chats, Jev records and Knowledge proposals that quote the content are counted only when that person opens the plan. " + purgeExposure}, nil
	}, "notes:manage")
}

// purgeFlowKind is the kind of the plans agents prepared for project. A
// notebook ID holds no '/', so each notebook's plans are their own group,
// whose bound (prepare_purge) another notebook's agent cannot reach.
func purgeFlowKind(project string) string { return "purge-plan-" + project }

// purgeTarget checks that the notebook can be purged by who asks: a notebook
// of notes kept in the database, which an Owner or Admin may edit.
func (s *Server) purgeTarget(ctx context.Context, project string, store *workspace.Store) (purgeTarget, error) {
	user := subject(ctx)
	if store == nil || project == "" {
		return purgeTarget{}, workspace.ErrForbidden
	}
	if _, err := s.catalog.Access(user, project, "owner"); err != nil {
		return purgeTarget{}, err
	}
	// A repository notebook mirrors its source and a task queue is written
	// only by its actions; neither is purged here.
	if _, err := s.catalog.Access(user, project, "edit"); err != nil {
		return purgeTarget{}, err
	}
	return purgeTarget{project: project, store: store, user: user}, nil
}

// purgeScope returns the scope a request names: its own, or that of the plan
// an agent prepared for this person under token.
func (s *Server) purgeScope(ctx context.Context, target purgeTarget, scope workspace.PurgeScope, token string) (workspace.PurgeScope, error) {
	if token == "" {
		return scope, nil
	}
	raw, err := s.db.ReadFlow(ctx, purgeFlowKind(target.project), token)
	if errors.Is(err, pg.ErrNoFlow) {
		return scope, webcore.Error{Status: http.StatusNotFound, Code: "not_found", Message: "This purge plan expired or was already used; prepare the purge again."}
	}
	if err != nil {
		return scope, err
	}
	var flow struct {
		Project string               `json:"project"`
		Scope   workspace.PurgeScope `json:"scope"`
	}
	if json.Unmarshal(raw, &flow) != nil || flow.Project != target.project {
		return scope, workspace.ErrForbidden
	}
	return flow.Scope, nil
}

// checkPurgeScope refuses a scope larger than a purge needs: a plan an
// agent prepares is stored, scope and all, until it expires.
func checkPurgeScope(scope workspace.PurgeScope) error {
	if len(scope.Text) > maxPurgeText || len(scope.Pattern) > maxPurgeText || len(scope.Paths) > maxPurgePaths || len(scope.Versions) > maxPurgePaths {
		return webcore.Error{Status: http.StatusRequestEntityTooLarge, Code: "too_large", Message: fmt.Sprintf("A purge's text or pattern is at most %d KiB, and it names at most %d notes or versions.", maxPurgeText>>10, maxPurgePaths)}
	}
	return nil
}

// purgeError classifies the workspace's purge errors for every transport.
func purgeError(err error) error {
	switch {
	case errors.Is(err, workspace.ErrPurgeConfirmation):
		return webcore.Error{Status: http.StatusConflict, Code: "conflict", Message: err.Error()}
	case errors.Is(err, workspace.ErrPurgeInvalid):
		return webcore.Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: err.Error()}
	case errors.Is(err, errors.ErrUnsupported):
		return webcore.Error{Status: http.StatusBadRequest, Code: "unsupported", Message: err.Error()}
	}
	return err
}

// preparePurge plans scope on the notebook and, for a person, counts what
// quoted the purged content outside it; an agent's plan counts nothing there
// (purgeDerived).
func (s *Server) preparePurge(ctx context.Context, target purgeTarget, scope workspace.PurgeScope, agent bool) (purgePlanView, error) {
	if err := checkPurgeScope(scope); err != nil {
		return purgePlanView{}, err
	}
	plan, err := target.store.PreparePurge(ctx, scope, target.store.PurgeExporter())
	if err != nil {
		return purgePlanView{}, err
	}
	view := purgePlanView{PurgePlan: s.visiblePlan(ctx, target, plan), Exposure: purgeExposure, NotebookUnchanged: !purgeHeld(plan)}
	if previousReleaseFiles(target.store) {
		view.Warnings = append(view.Warnings, purgePreviousRelease)
	}
	if agent {
		view.RecordsCountedOnReview = true
		return view, nil
	}
	cleaner, quotes, err := s.purgeCleanerFor(ctx, target, plan)
	if err != nil {
		return purgePlanView{}, err
	}
	derived, err := s.purgeOwnRecords(ctx, target, cleaner)
	if err != nil {
		return purgePlanView{}, err
	}
	derived.Partial = quotes.Partial
	view.Derived = &derived
	view.RecordsUnsearched = plan.Scope.Kind == workspace.PurgeText && cleaner.text == nil
	return view, nil
}

// purgeOwnRecords counts what quoted the purged content outside the
// notebook as target's caller may be told: every record for an instance
// Admin, and otherwise only the caller's own (purgeDerived).
func (s *Server) purgeOwnRecords(ctx context.Context, target purgeTarget, cleaner *purgeCleaner) (purgeDerived, error) {
	if s.catalog.IsAdmin(target.user) {
		return s.purgeDerivedRecords(ctx, target, cleaner, "", false, false)
	}
	own := *cleaner
	own.owner = s.purgeRecordOwner(target.user)
	derived, err := s.purgeDerivedRecords(ctx, target, &own, "", false, false)
	if !cleaner.empty() {
		derived.Own, derived.Others = true, purgeOthersRedacted
	}
	return derived, err
}

// purgeRecordOwner reports whether a record outside the notebook, as a
// purge's clean-up reads it, is user's own: a chat transcript, Jev job or
// citation check they own, a Knowledge proposal they made, a consumer
// checkpoint of theirs, or a result of a Jev job of theirs.
func (s *Server) purgeRecordOwner(user string) func(raw []byte) bool {
	jobs := map[string]bool{}
	return func(raw []byte) bool {
		var record struct {
			Owner  string `json:"owner"`
			Author string `json:"author"`
			User   string `json:"user"`
			Job    string `json:"job"`
		}
		if json.Unmarshal(raw, &record) != nil {
			return false
		}
		if record.Owner != "" || record.Author != "" || record.User != "" {
			return record.Owner == user || record.Author == user || record.User == user
		}
		if record.Job == "" || s.jevState == nil || s.jevState.records == nil {
			return false
		}
		mine, known := jobs[record.Job]
		if !known {
			found, err := s.jevState.records.ListMatching(jobName(record.Job), map[string]any{"owner": user}, 1)
			mine = err == nil && len(found) > 0
			jobs[record.Job] = mine
		}
		return mine
	}
}

// previousReleaseFiles reports whether the data directory still holds the
// previous release's files for store's notebook: its Git working tree and
// repository at Root, and the live edits, captures and write journal beside
// it, none of which this release writes.
func previousReleaseFiles(store *workspace.Store) bool {
	state := filepath.Dir(store.Root)
	for _, path := range []string{filepath.Join(store.Root, ".git"), filepath.Join(state, "collaboration"), filepath.Join(state, "captures"), filepath.Join(state, "write-journal.json")} {
		if _, err := os.Lstat(path); err == nil {
			return true
		}
	}
	return false
}

// purgeHeld reports whether the notebook holds what plan erases, in its
// history or its live documents.
func purgeHeld(plan workspace.PurgePlan) bool { return plan.From != "" || len(plan.Live) > 0 }

// visiblePlan returns plan as who asks may see it. Another notebook that
// keeps content being erased is listed only when they may read it, through
// the credential they use: whether a notebook they cannot read holds a file
// they guess is not theirs to learn, and the purge keeps what other
// notebooks use anyway. Its confirmation binds what it shows
// (purgeConfirmation), never what it leaves out.
func (s *Server) visiblePlan(ctx context.Context, target purgeTarget, plan workspace.PurgePlan) workspace.PurgePlan {
	plan.Elsewhere, plan.Shared = s.visibleOccurrences(ctx, target.user, plan.Elsewhere), s.visibleOccurrences(ctx, target.user, plan.Shared)
	plan.Confirmation = purgeConfirmation(target.project, plan)
	return plan
}

func (s *Server) visibleOccurrences(ctx context.Context, user string, list []workspace.PurgeOccurrence) []workspace.PurgeOccurrence {
	var out []workspace.PurgeOccurrence
	for _, o := range list {
		if o.Kind == "notebook" {
			if _, err := s.catalog.Access(user, o.Path, "read"); err != nil || !s.clientProjectAllowed(ctx, o.Path) {
				continue
			}
		}
		out = append(out, o)
	}
	return out
}

// purgeConfirmation binds a plan, as its caller sees it, to its notebook,
// head, scope and everything it reports, as PurgePlan.Bound returns it: so
// whether the purge changes the notebook at all (purgeHeld) is bound, as is
// every live document whose edits not saved as a revision the purge would
// discard, and every live document a purge that rewrites no revision
// resets. The other live documents a purge that rewrites revisions resets,
// which come and go as people open notes, are not, and its receipt counts
// every one it reset. The workspace binds the whole plan likewise with its
// own confirmation, which never leaves the server: a hash of a plan whose
// hidden parts a caller can guess would tell them.
func purgeConfirmation(project string, plan workspace.PurgePlan) string {
	raw, _ := json.Marshal(struct {
		Notebook string              `json:"notebook"`
		Plan     workspace.PurgePlan `json:"plan"`
	}{project, plan.Bound()})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// purgeReceiptFor returns receipt as user may see it. What storage deleted
// depends on what other notebooks keep, so only an instance Admin sees it;
// anyone else is told the versions erased from this notebook.
//
// Counts of the records outside the notebook that quoted the content are
// likewise an Admin's: the person who ran the purge sees their own
// records' counts (ActorDerived), and anyone else no counts. Its warnings
// say without a number that records could not be redacted, which may be
// other people's.
func (s *Server) purgeReceiptFor(user string, receipt purgeReceipt) purgeReceipt {
	derived := receipt.ActorDerived
	receipt.ActorDerived = nil
	if s.catalog.IsAdmin(user) {
		return receipt
	}
	receipt.RemovedVersions, receipt.RemovedBlocks = receipt.Versions, 0
	receipt.Warnings = withoutRecordCounts(receipt.Warnings)
	switch {
	case derived != nil && receipt.Actor == user:
		receipt.Derived = *derived
	case receipt.Unfinished || receipt.Interrupted:
		// Its warnings say what may not have been redacted.
		receipt.Derived = purgeDerived{Withheld: true}
	default:
		receipt.Derived = purgeDerived{Withheld: true, Others: purgeOthersWithheld}
	}
	return receipt
}

// unredactedCount matches what chat.RedactProject and decision.RedactNotebook
// say of the records they could not redact: how many of everyone's records
// quoted the content, which only an Admin is told.
var unredactedCount = regexp.MustCompile(`\b\d+ (chat transcripts|Jev records) cannot be redacted and are left as they are`)

// withoutRecordCounts returns warnings with those counts replaced by a
// sentence without a number.
func withoutRecordCounts(warnings []string) []string {
	if warnings == nil {
		return nil
	}
	out := make([]string, len(warnings))
	for i, warning := range warnings {
		out[i] = unredactedCount.ReplaceAllString(warning, "some $1, perhaps other people's, cannot be redacted and are left as they are; only an instance Admin is told how many")
	}
	return out
}

// purgeCleanerFor returns what redacts plan's content outside the notebook,
// read now, before the purge removes it: what the notebook holds of it
// (PurgeQuotes). An instance Admin, who may redact anyone's records, has a
// text purge match its text or pattern itself. Only a person's plan and
// purge use it.
func (s *Server) purgeCleanerFor(ctx context.Context, target purgeTarget, plan workspace.PurgePlan) (*purgeCleaner, workspace.PurgeQuotes, error) {
	if plan.Scope.Kind == workspace.PurgeText && s.catalog.IsAdmin(target.user) {
		redactor, err := workspace.NewPurgeRedactor(plan.Scope)
		if err != nil {
			return nil, workspace.PurgeQuotes{}, err
		}
		return &purgeCleaner{text: &redactor}, workspace.PurgeQuotes{}, nil
	}
	quotes, err := target.store.PurgeQuotes(ctx, plan)
	if err != nil {
		return nil, quotes, err
	}
	return &purgeCleaner{text: quotes.Text, lines: quotes.Lines}, quotes, nil
}

// purgeDerivedRecords counts, or with write redacts, what quoted the purged
// content outside the notebook: the private chat transcripts of the notebook
// and of other notebooks that drew on it, its Jev records and its Knowledge
// proposals. With write it also clears the notebook's search and semantic
// index on every gateway after purge, and drops what held the content
// before it: plans agents prepared, bundle exports in progress and the
// bundles this gateway is preparing or sending.
func (s *Server) purgeDerivedRecords(ctx context.Context, target purgeTarget, cleaner *purgeCleaner, purge string, write, index bool) (purgeDerived, error) {
	var out purgeDerived
	var errs []error
	if cleaner.empty() && !write {
		return out, nil
	}
	if s.chat != nil && s.chat.Store != nil {
		n, err := s.chat.Store.RedactProject(ctx, target.project, cleaner.document, write)
		out.Chats = n
		if err != nil {
			errs = append(errs, fmt.Errorf("chat transcripts: %w", err))
		}
	}
	if s.jevState != nil && s.jevState.records != nil {
		n, err := s.jevState.records.RedactNotebook(ctx, target.project, cleaner.document, write)
		out.JevRecords = n
		if err != nil {
			errs = append(errs, fmt.Errorf("Jev records: %w", err))
		}
	}
	if !write {
		var err error
		out.Proposals, err = withEngine(s, ctx, target.store, func(e *knowledge.Engine) (int, error) {
			return e.PurgeHolds(ctx, cleaner.document)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("Knowledge proposals: %w", err))
		}
		return out, errors.Join(errs...)
	}
	cleanup, err := s.indexes.Purged(ctx, target.store, purge, cleaner.document, index)
	out.Proposals, out.Checkpoints, out.Vectors = cleanup.Proposals, cleanup.Checkpoints, cleanup.Vectors
	if err != nil {
		errs = append(errs, fmt.Errorf("search and Knowledge records: %w", err))
	}
	// Plans an agent prepared for this notebook hold the scope, which may be
	// the purged text.
	if _, err := s.db.Pool.Exec(ctx, `DELETE FROM pending_flows WHERE org = $1 AND kind = $2`, s.db.Org, purgeFlowKind(target.project)); err != nil {
		errs = append(errs, fmt.Errorf("prepared purge plans: %w", err))
	}
	if err := s.dropBundleExports(ctx); err != nil {
		errs = append(errs, fmt.Errorf("bundle exports in progress: %w", err))
	}
	if err := s.removeTemporaryBundles(); err != nil {
		errs = append(errs, fmt.Errorf("temporary bundles: %w", err))
	}
	return out, errors.Join(errs...)
}

// fenceExports ends every bundle export in progress, and keeps a new one
// from recording itself until release runs; a purge holds it while it
// rewrites a notebook's history. Each chunk an export serves is read only
// while its row is there, and dropBundleExports removes a bundle after its
// row, so no export serves a chunk of the notebook as it was once the
// rewrite has committed. An export that made its bundle before the purge
// and records it after finds the commits it names rewritten and ends itself
// (beginExport).
func (s *Server) fenceExports(ctx context.Context) (release func(), err error) {
	if release, err = s.db.Exclusive(ctx, "exports"); err != nil {
		return nil, fmt.Errorf("end the bundle exports in progress: %w", err)
	}
	if err = s.dropBundleExports(ctx); err != nil {
		release()
		return nil, fmt.Errorf("end the bundle exports in progress: %w", err)
	}
	return release, nil
}

// dropBundleExports ends every bundle export in progress, whose bundle holds
// its notebook as it was; its client starts again. An export's row does not
// name its notebook, so the instance's few are all ended.
func (s *Server) dropBundleExports(ctx context.Context) error {
	rows, err := s.db.Pool.Query(ctx, `DELETE FROM exports WHERE org = $1 RETURNING id`, s.db.Org)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := s.blobs.Delete(ctx, exportKey(id)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// executePurge runs the plan in confirms, then cleans the derived records and
// keeps the receipt. The plan is made again first: it must still be the one
// confirmed, and a note or versions purge reads the versions it removes for
// the lines that quoted them elsewhere.
func (s *Server) executePurge(ctx context.Context, target purgeTarget, in purgeExecuteInput) (purgeReceipt, error) {
	if err := checkPurgeScope(in.Scope); err != nil {
		return purgeReceipt{}, err
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return purgeReceipt{}, webcore.Invalid("A purge needs a reason, which its receipt keeps.")
	}
	if len(reason) > 1000 || strings.ContainsRune(reason, 0) {
		return purgeReceipt{}, webcore.Invalid("The reason must be at most 1000 characters.")
	}
	actor := target.user
	if operator := strings.TrimSpace(in.Operator); operator != "" {
		if !s.catalog.IsAdmin(actor) || len(operator) > 200 || strings.ContainsAny(operator, "\x00\n\r") {
			return purgeReceipt{}, webcore.Invalid("Only the instance access key names an operator, in one line of at most 200 characters.")
		}
		actor += " (operator: " + operator + ")"
	}
	store := target.store
	export := store.PurgeExporter()
	plan, err := store.PreparePurge(ctx, in.Scope, export)
	switch {
	case err != nil:
		return purgeReceipt{}, err
	case plan.Revision != in.Revision:
		return purgeReceipt{}, fmt.Errorf("%w: the notebook changed since the purge was planned; review the plan again", workspace.ErrConflict)
	case in.Confirmation == "" || s.visiblePlan(ctx, target, plan).Confirmation != in.Confirmation:
		return purgeReceipt{}, workspace.ErrPurgeConfirmation
	case plan.Blocked != "":
		return purgeReceipt{}, fmt.Errorf("%w: %s", workspace.ErrPurgeBlocked, plan.Blocked)
	}
	held := purgeHeld(plan)
	if !held && !s.catalog.IsAdmin(target.user) {
		// Nothing in the notebook holds the content; what quoted it may. Only
		// an Admin redacts other people's records of text the notebook does
		// not hold.
		return purgeReceipt{}, webcore.Error{Status: http.StatusForbidden, Code: "forbidden", Message: "Nothing in this notebook holds this content. Only an instance Admin can redact the records outside the notebook that quoted it."}
	}
	cleaner, quotes, err := s.purgeCleanerFor(ctx, target, plan)
	if err != nil {
		return purgeReceipt{}, err
	}
	if purgeReasonQuotes(reason, plan.Scope, quotes) {
		return purgeReceipt{}, webcore.Invalid("The reason holds the content being purged, or a word or number that only it holds, such as a date, an ID or a key; say why without quoting it.")
	}
	// Someone who is not an Admin is told only of their own records, which
	// are counted now, while they still quote the content.
	var own *purgeDerived
	if !s.catalog.IsAdmin(target.user) {
		counted, err := s.purgeOwnRecords(ctx, target, cleaner)
		if err != nil {
			return purgeReceipt{}, err
		}
		counted.Partial = quotes.Partial
		own = &counted
	}
	// The confirmed purge runs to its end whether or not its request does,
	// as when the person reloads or closes the tab: once the history is
	// rewritten, the records that quoted it can be found only now, and a
	// note or versions purge cannot be planned again to clean them.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), purgeWork)
	defer cancel()
	receipt := purgeReceipt{Notebook: target.project, Actor: actor, Reason: reason, Kind: in.Scope.Kind, Pattern: in.Scope.Pattern != "",
		WholeNotebook: in.Scope.Kind == workspace.PurgeText && len(in.Scope.Paths) == 0, Notes: len(plan.Notes), Versions: plan.Versions,
		Messages: plan.Messages, PreparedByAgent: in.Token != "", Exposure: purgeExposure, ActorDerived: own}
	if name, err := s.catalog.SubjectName(target.user); err == nil {
		receipt.ActorName = name
	}
	var result workspace.PurgeResult
	if !held {
		found, err := s.purgeDerivedRecords(ctx, target, cleaner, "", false, false)
		if err != nil {
			return purgeReceipt{}, err
		}
		if found.Chats+found.JevRecords+found.Proposals == 0 {
			return purgeReceipt{}, fmt.Errorf("%w: nothing holds the content to purge", workspace.ErrPurgeInvalid)
		}
		result = workspace.PurgeResult{Purge: "purge-" + RandomToken()[:32], Revision: plan.Revision}
		receipt.NotebookUnchanged = true
	} else {
		// Held until the receipt is kept: a recorded purge without one whose
		// lock is free stopped before it finished (finishStoppedPurges).
		running, err := s.db.Exclusive(ctx, purgeRunKey(target.project))
		if err != nil {
			return purgeReceipt{}, err
		}
		defer running()
		// Bundle exports in progress hold the notebook as it is now: they end
		// before its history is rewritten, once the purge has passed every
		// check, and none records itself meanwhile.
		release := func() {}
		fence := func(ctx context.Context) error {
			end, err := s.fenceExports(ctx)
			if err == nil {
				release = end
			}
			return err
		}
		purging := func() {}
		if export != nil {
			// The purge rebuilds its export itself; an export that finds the
			// history rewritten meanwhile leaves it to the purge.
			purging = export.Purging(target.project)
		}
		result, err = store.ExecutePurgeFenced(ctx, plan, plan.Confirmation, actor, reason, export, fence)
		purging()
		release()
		if err != nil && result.Purge == "" {
			return purgeReceipt{}, err
		}
		if err != nil {
			// The history was rewritten; what failed after it is reported,
			// and the records outside the notebook are cleaned all the same.
			receipt.Warnings = append(receipt.Warnings, err.Error())
		}
		cleaner.remap = s.purgeRemap(ctx, store)
	}
	receipt.Purge, receipt.Revision, receipt.Rewritten = result.Purge, result.Revision, result.Rewritten
	receipt.RemovedVersions, receipt.RemovedBlocks, receipt.LiveReset = result.RemovedVersions, result.RemovedBlocks, result.LiveReset
	receipt.Export, receipt.ExportForcePush = result.Export, result.ExportForcePush
	receipt.Created = time.Now().UTC().Format(time.RFC3339)
	if previousReleaseFiles(store) {
		receipt.Warnings = append(receipt.Warnings, purgePreviousRelease)
	}
	derived, err := s.purgeDerivedRecords(ctx, target, cleaner, result.Purge, true, !receipt.NotebookUnchanged)
	derived.Partial = quotes.Partial
	receipt.Derived = derived
	if err != nil {
		receipt.Warnings = append(receipt.Warnings, "Some records outside the notebook could not be cleaned: "+err.Error())
	}
	if err := s.keepPurgeReceipt(ctx, receipt); err != nil {
		// The purge finished: until its receipt is kept, a gateway that
		// starts would take it for one that stopped, and keep a receipt
		// saying it did not redact what it did.
		if spoolErr := s.spoolPurgeReceipt(receipt); spoolErr != nil {
			receipt.Warnings = append(receipt.Warnings, "The purge receipt could not be kept: "+errors.Join(err, spoolErr).Error())
		} else {
			receipt.Warnings = append(receipt.Warnings, "The purge receipt could not be kept in the database yet ("+err.Error()+"); this gateway keeps it there once it can, at the latest when it next starts, and the purge is listed as unfinished until then.")
		}
	}
	return s.purgeReceiptFor(target.user, receipt), nil
}

// purgeReceiptSpool is where a gateway keeps the receipts of purges that
// finished whose receipt the database refused, until it takes them
// (keepSpooledPurgeReceipts).
func (s *Server) purgeReceiptSpool() string { return filepath.Join(s.data, "purge-receipts") }

func (s *Server) spooledPurgeReceiptPath(notebook, purge string) string {
	sum := sha256.Sum256([]byte(notebook + "/" + purge))
	return filepath.Join(s.purgeReceiptSpool(), hex.EncodeToString(sum[:])+".json")
}

// spoolPurgeReceipt keeps receipt in the gateway's data directory.
func (s *Server) spoolPurgeReceipt(receipt purgeReceipt) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := durable.MkdirAll(s.purgeReceiptSpool(), 0o700, nil); err != nil {
		return err
	}
	return durable.WriteFile(s.spooledPurgeReceiptPath(receipt.Notebook, receipt.Purge), ".receipt-", raw, nil)
}

// keepSpooledPurgeReceipts keeps in the database the receipts this gateway
// spooled, each in place of an interrupted receipt another gateway kept
// meanwhile, and returns the purges whose receipt is still only spooled.
func (s *Server) keepSpooledPurgeReceipts(ctx context.Context) map[string]bool {
	waiting := map[string]bool{}
	entries, err := os.ReadDir(s.purgeReceiptSpool())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("reading the purge receipts not yet kept", "error", err)
		}
		return waiting
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.purgeReceiptSpool(), entry.Name())
		raw, err := os.ReadFile(path)
		var receipt purgeReceipt
		if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.Notebook == "" || receipt.Purge == "" || path != s.spooledPurgeReceiptPath(receipt.Notebook, receipt.Purge) {
			slog.Warn("a purge receipt not yet kept cannot be read", "file", path, "error", err)
			continue
		}
		err = s.keepPurgeReceipt(ctx, receipt)
		if errors.Is(err, pg.ErrConflict) {
			// The database holds a receipt of the purge: its own, kept
			// already, or an interrupted one, which keeping this one in its
			// place lost to a concurrent write; that waits for the next
			// sweep.
			err = s.purgeReceiptFinal(ctx, receipt)
		}
		if err == nil {
			err = os.Remove(path)
		}
		if err != nil {
			waiting[receipt.Notebook+"/"+receipt.Purge] = true
			slog.Warn("keeping a purge receipt", "notebook", receipt.Notebook, "purge", receipt.Purge, "error", err)
		}
	}
	return waiting
}

// purgeReceiptFinal returns nil when the database holds a receipt of
// receipt's purge that is not an interrupted one.
func (s *Server) purgeReceiptFinal(ctx context.Context, receipt purgeReceipt) error {
	version, raw, err := s.db.ReadDocument(ctx, s.db.Pool, purgeReceiptKind, receipt.Notebook+"/"+receipt.Purge, -1)
	if err != nil {
		return err
	}
	var kept purgeReceipt
	if version == 0 || json.Unmarshal(raw, &kept) != nil || kept.Interrupted {
		return fmt.Errorf("%w: the database holds no finished receipt of the purge yet", pg.ErrConflict)
	}
	return nil
}

// purgeReasonQuotes reports whether a purge's reason quotes what it purges:
// a text purge's text or a match of its pattern, or what the notebook holds
// of what it erases, even a single identifying word.
func purgeReasonQuotes(reason string, scope workspace.PurgeScope, quotes workspace.PurgeQuotes) bool {
	if scope.Kind == workspace.PurgeText {
		if redactor, err := workspace.NewPurgeRedactor(scope); err == nil && redactor.Holds(reason) {
			return true
		}
	}
	return quotes.Quotes(reason)
}

// purgeRemap returns what maps a revision the purge rewrote to the revision
// that stands for it now, remembering each answer.
func (s *Server) purgeRemap(ctx context.Context, store *workspace.Store) func(string) (string, bool) {
	known := map[string]string{}
	return func(revision string) (string, bool) {
		if now, ok := known[revision]; ok {
			return now, now != ""
		}
		now, purged, err := store.ResolvePurgedRevision(ctx, "", revision)
		if err != nil || purged || now == revision {
			now = ""
		}
		known[revision] = now
		return now, now != ""
	}
}

// keepPurgeReceipt stores receipt, which holds no purged content, for the
// notebook's audit.
func (s *Server) keepPurgeReceipt(ctx context.Context, receipt purgeReceipt) error {
	body, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	id := receipt.Notebook + "/" + receipt.Purge
	_, err = s.db.WriteDocument(ctx, s.db.Pool, purgeReceiptKind, id, 0, body)
	if errors.Is(err, pg.ErrConflict) && !receipt.Interrupted {
		// A gateway that started while this purge ran took it for stopped,
		// as when the connection holding its lock dropped; the purge's own
		// receipt replaces the one that gateway kept.
		version, raw, readErr := s.db.ReadDocument(ctx, s.db.Pool, purgeReceiptKind, id, -1)
		var kept purgeReceipt
		if readErr == nil && version > 0 && json.Unmarshal(raw, &kept) == nil && kept.Interrupted {
			_, err = s.db.WriteDocument(ctx, s.db.Pool, purgeReceiptKind, id, version, body)
		}
	}
	return err
}

// stoppedPurges lists the recorded purges of project, or of every notebook
// for "", that kept no receipt: each is still running, or stopped after it
// rewrote the notebook, before it finished.
func (s *Server) stoppedPurges(ctx context.Context, project string) ([]notestore.RecordedPurge, error) {
	recorded, err := notestore.RecordedPurges(ctx, s.db, project)
	if err != nil || len(recorded) == 0 {
		return nil, err
	}
	ids, err := s.db.DocumentIDs(ctx, s.db.Pool, purgeReceiptKind)
	if err != nil {
		return nil, err
	}
	var out []notestore.RecordedPurge
	for _, p := range recorded {
		if _, kept := slices.BinarySearch(ids, p.Notebook+"/"+p.ID); !kept {
			out = append(out, p)
		}
	}
	return out, nil
}

// recordedPurgeReceipt is what the notebook recorded of purge p, as a
// receipt: who, when, why and what kind, without what it changed.
func recordedPurgeReceipt(p notestore.RecordedPurge) purgeReceipt {
	var scope struct {
		Kind     string   `json:"kind"`
		Pattern  bool     `json:"pattern"`
		Notebook bool     `json:"notebook"`
		Entries  []string `json:"entries"`
		Versions []string `json:"versions"`
		Messages int      `json:"messages"`
	}
	json.Unmarshal(p.Scope, &scope)
	return purgeReceipt{Purge: p.ID, Notebook: p.Notebook, Actor: p.Actor, Reason: p.Reason, Created: p.Created.UTC().Format(time.RFC3339),
		Kind: scope.Kind, Pattern: scope.Pattern, WholeNotebook: scope.Notebook, Notes: len(scope.Entries), Versions: len(scope.Versions), Messages: scope.Messages, Exposure: purgeExposure}
}

// stoppedPurgeWarnings says what purge p, which stopped after it rewrote
// the notebook, did not do that needs a person, and how to do it.
func stoppedPurgeWarnings(p notestore.RecordedPurge) []string {
	var out []string
	if p.FromSeq > 0 {
		out = append(out, fmt.Sprintf("Its Git export is rebuilt for this purge, and flagged so the backup worker replaces the GitHub copy once, the next time the gateway that runs the export exports the notebook (within five minutes); with the instance stopped, `forgetmenote admin purge rebuild -notebook %s -purge %s` does it.", p.Notebook, p.ID))
	}
	return append(out, "The private chat transcripts, Knowledge proposals and Jev records that quoted the erased content may not have been redacted. An instance Admin can redact them by purging the erased text, or each erased line of a note or versions purge, as text: a plan that finds it only outside the notebook redacts it there.")
}

// finishStoppedPurges finishes what it can of each purge that rewrote a
// notebook and stopped before it kept its receipt, as when the gateway
// running it stopped. What needs no erased content runs again: the
// notebook's search and Knowledge indexes are reset on every gateway, and
// the plans agents prepared for it and the bundle exports in progress end.
// Then it keeps a receipt that says the purge was interrupted and what it
// did not do. The export is the exporter's, which rebuilds it for the purge
// when it next exports the notebook. A purge whose lock another gateway
// holds is still running there. It runs as the gateway starts.
//
// A purge that finished but whose receipt the database refused kept it in
// this gateway's data directory: it is kept first, and such a purge is not
// taken for stopped.
func (s *Server) finishStoppedPurges(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	spooled := s.keepSpooledPurgeReceipts(ctx)
	stopped, err := s.stoppedPurges(ctx, "")
	if err != nil {
		slog.Warn("listing purges that stopped before they finished", "error", err)
		return
	}
	byNotebook := map[string][]notestore.RecordedPurge{}
	for _, p := range stopped {
		if !spooled[p.Notebook+"/"+p.ID] {
			byNotebook[p.Notebook] = append(byNotebook[p.Notebook], p)
		}
	}
	for _, notebook := range slices.Sorted(maps.Keys(byNotebook)) {
		if err := s.finishStoppedPurgesOf(ctx, notebook, byNotebook[notebook]); err != nil {
			slog.Warn("finishing purges that stopped before they finished", "notebook", notebook, "error", err)
		}
	}
}

func (s *Server) finishStoppedPurgesOf(ctx context.Context, notebook string, purges []notestore.RecordedPurge) error {
	release, free, err := s.db.TryExclusive(ctx, purgeRunKey(notebook))
	if err != nil || !free {
		return err
	}
	defer release()
	store, err := s.catalog.Access(catalog.Bootstrap, notebook, "read")
	if err != nil {
		return err
	}
	keep := func(raw []byte) ([]byte, bool, error) { return raw, false, nil }
	for _, p := range purges {
		// It may have kept its receipt since it was listed.
		if version, _, err := s.db.ReadDocument(ctx, s.db.Pool, purgeReceiptKind, notebook+"/"+p.ID, -1); err != nil || version > 0 {
			if err != nil {
				return err
			}
			continue
		}
		if _, err := s.indexes.Purged(ctx, store, p.ID, keep, true); err != nil {
			return err
		}
		if _, err := s.db.Pool.Exec(ctx, `DELETE FROM pending_flows WHERE org = $1 AND kind = $2`, s.db.Org, purgeFlowKind(notebook)); err != nil {
			return err
		}
		if err := errors.Join(s.dropBundleExports(ctx), s.removeTemporaryBundles()); err != nil {
			return err
		}
		receipt := recordedPurgeReceipt(p)
		receipt.Interrupted = true
		receipt.Warnings = append([]string{"This purge stopped after it rewrote the notebook, before it finished. A gateway that started since reset the notebook's search and Knowledge indexes on every gateway, and ended the plans agents prepared for it and the bundle exports in progress."}, stoppedPurgeWarnings(p)...)
		if previousReleaseFiles(store) {
			receipt.Warnings = append(receipt.Warnings, purgePreviousRelease)
		}
		if err := s.keepPurgeReceipt(ctx, receipt); err != nil {
			return err
		}
	}
	return nil
}

// purgeReceipts lists the notebook's purge receipts, newest first, with an
// unfinished entry for each purge that kept none.
func (s *Server) purgeReceipts(ctx context.Context, project string) ([]purgeReceipt, error) {
	ids, err := s.db.DocumentIDs(ctx, s.db.Pool, purgeReceiptKind)
	if err != nil {
		return nil, err
	}
	receipts := []purgeReceipt{}
	for _, id := range ids {
		if !strings.HasPrefix(id, project+"/") {
			continue
		}
		version, raw, err := s.db.ReadDocument(ctx, s.db.Pool, purgeReceiptKind, id, -1)
		if err != nil {
			return nil, err
		}
		var receipt purgeReceipt
		if version == 0 || json.Unmarshal(raw, &receipt) != nil {
			continue
		}
		receipts = append(receipts, receipt)
	}
	stopped, err := s.stoppedPurges(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, p := range stopped {
		receipt := recordedPurgeReceipt(p)
		receipt.Unfinished = true
		receipt.Warnings = append([]string{"This purge rewrote the notebook and has kept no receipt: it is still running, or the gateway running it stopped before it finished. A gateway that starts finishes what it can and keeps its receipt."}, stoppedPurgeWarnings(p)...)
		receipts = append(receipts, receipt)
	}
	slices.SortFunc(receipts, func(a, b purgeReceipt) int { return strings.Compare(b.Created+b.Purge, a.Created+a.Purge) })
	return receipts, nil
}
