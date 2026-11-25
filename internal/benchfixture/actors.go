package benchfixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// Actor is one simulated writer. Humans write through the browser, directly or
// through live collaboration checkpoints; the assistant is an MCP service
// account; the organizer is the owner's Jev organizer. Their identities and
// trailers follow web/server.go, catalog/accounts.go and workspace/db_commit.go.
type Actor struct {
	Key     string // stable short name
	Agent   bool
	Subject string // catalog subject: comment author, Trash deletedBy
	Account string // 32-hex account ID, the Gitnotes-User trailer
	Name    string // Git author name
	Email   string // Git author email
	Source  string // Gitnotes-Source
	Client  string // Gitnotes-Client
	Zone    *time.Location
}

func accountID(key string) string { return newRNG(0, "account:"+key).hexID(16) }

func localActor(key string, offsetHours int) Actor {
	id := accountID(key)
	return Actor{Key: key, Subject: key, Account: id, Name: "User " + id[:12], Email: id + "@users.gitnotes.invalid", Source: "browser", Zone: time.FixedZone("", offsetHours*3600)}
}

// Actors are the three humans and two agents of every fixture. Ada owns the
// notebooks; Ben signs his commits with his GitHub identity.
var Actors = func() []Actor {
	ada, ben, chen := localActor("ada", 1), localActor("ben", -5), localActor("chen", 8)
	ben.Name, ben.Email = "benokafor", "benokafor@users.noreply.github.com"
	assistant := localActor("assistant", 0)
	assistant.Agent, assistant.Name, assistant.Source, assistant.Client = true, "Automation "+assistant.Account[:12], "mcp", "claude-code"
	organizer := ada
	organizer.Key, organizer.Agent, organizer.Source, organizer.Client, organizer.Zone = "organizer", true, "jev", "organizer", time.UTC
	return []Actor{ada, ben, chen, assistant, organizer}
}()

const (
	actorAda = iota
	actorBen
	actorChen
	actorAssistant
	actorOrganizer
)

// ActorByKey returns the actor with key, or false.
func ActorByKey(key string) (Actor, bool) {
	i := slices.IndexFunc(Actors, func(a Actor) bool { return a.Key == key })
	if i < 0 {
		return Actor{}, false
	}
	return Actors[i], true
}

func contentHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// sign sets the author and trailers of an ordinary save by actor a.
// Operations (Jev organizer actions) also carry Gitnotes-Operation/Request.
func (a Actor) sign(r *Revision, operation string) {
	r.Actor, r.AuthorName, r.AuthorEmail = a.Key, a.Name, a.Email
	r.Time = r.Time.In(a.Zone)
	r.Trailers = []Trailer{{"Gitnotes-Source", a.Source}, {"Gitnotes-User", a.Account}}
	if a.Client != "" {
		r.Trailers = append(r.Trailers, Trailer{"Gitnotes-Client", a.Client})
	}
	if operation != "" {
		r.Trailers = append(r.Trailers, Trailer{"Gitnotes-Operation", contentHash(a.Account, operation)}, Trailer{"Gitnotes-Request", contentHash(operation, "request")})
	}
}

// signLive makes r the checkpoint of a's accepted live edits based on revision
// base, authored by the collaboration identity with a receipt trailer.
func (a Actor) signLive(r *Revision, path, base string) {
	r.Actor, r.AuthorName, r.AuthorEmail = a.Key, "ForgetMeNote collaboration", CommitterEmail
	r.Time = r.Time.In(time.UTC)
	r.Subject = "Checkpoint live edits to " + quotePath(path)
	request := contentHash(path, base, r.Time.Format(time.RFC3339))
	type identity struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Email  string `json:"email"`
		Source string `json:"source,omitempty"`
		Client string `json:"client,omitempty"`
	}
	type liveReceipt struct {
		Request  string   `json:"request"`
		Revision string   `json:"revision"`
		Actor    identity `json:"actor"`
		Time     string   `json:"time"`
	}
	operation := contentHash(path, base, request, "live")
	raw, _ := json.Marshal(struct {
		Operation string      `json:"operation"`
		Receipt   liveReceipt `json:"receipt"`
	}{operation, liveReceipt{request, base, identity{a.Account, a.Name, a.Email, a.Source, a.Client}, r.Time.Add(-20 * time.Second).UTC().Format(time.RFC3339)}})
	r.Trailers = []Trailer{{"Gitnotes-Source", "collaboration"}, {"Gitnotes-Operation", contentHash(path, base, request)}, {"Gitnotes-Request", request}, {"Collaboration-Receipt", string(raw)}}
}

func quotePath(p string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(p) }
