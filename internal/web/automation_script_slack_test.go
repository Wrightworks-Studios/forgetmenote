package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"forgetmenote/internal/automation"
)

type slackPosts struct {
	mu        sync.Mutex
	bodies    []map[string]any
	uploads   []string // the bytes each private upload sent
	completes []map[string]any
}

// result decodes a host result the way the Python client receives it, whether
// it was just produced or replayed from the run journal.
func result(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (p *slackPosts) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.bodies)
}

// slackPostFixture installs a script granted messages.post (plus any other
// capabilities), an approved broadcast for its notification channel and a
// local Slack API.
func slackPostFixture(t *testing.T, code string, caps []string, connections map[string][]string, respond func(w http.ResponseWriter, n int)) (*Server, *scriptExecution, *slackPosts) {
	t.Helper()
	m := automation.Manifest{APIVersion: automation.PackageAPI, ID: "fixture", Name: "Fixture", Entrypoint: "main.py", Runtime: automation.PythonRuntime, Files: []string{"main.py"}, Capabilities: append([]string{"messages.post"}, caps...), Connections: connections}
	raw, _ := json.Marshal(m)
	app, e := packageFixtureWith(t, m, map[string]string{"automation.json": string(raw), "main.py": code}, func(spec *automation.Spec) { spec.NotifyChannel = "Cposts" })
	configureSlackFixture(t, app)
	app.slack.config.Broadcasts = map[string]SlackBroadcast{"Cposts": {Project: "default", Actors: []string{e.run.Spec.Actor}, Connections: []string{"warehouse"}, AudienceApproved: true}}
	posts := &slackPosts{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/files.getUploadURLExternal":
			if r.ParseForm() != nil || r.PostForm.Get("length") == "" {
				t.Error("upload URL request lacks its form fields")
			}
			posts.mu.Lock()
			uploaded := len(posts.uploads)
			posts.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "file_id": fmt.Sprintf("Fcard%d", uploaded), "upload_url": server.URL + "/upload"})
			return
		case "/upload":
			raw, _ := io.ReadAll(r.Body)
			posts.mu.Lock()
			posts.uploads = append(posts.uploads, string(raw))
			posts.mu.Unlock()
			w.Write([]byte("OK"))
			return
		case "/files.completeUploadExternal":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			posts.mu.Lock()
			posts.completes = append(posts.completes, body)
			posts.mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/chat.postMessage" || r.Header.Get("Authorization") != "Bearer fixture-bot" {
			t.Errorf("unexpected Slack call %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		posts.mu.Lock()
		posts.bodies = append(posts.bodies, body)
		n := len(posts.bodies)
		posts.mu.Unlock()
		respond(w, n)
	}))
	t.Cleanup(server.Close)
	app.slack.endpoint = server.URL + "/chat.postEphemeral"
	return app, e, posts
}

func TestScriptPostsThreadedSlackMessagesToApprovedChannel(t *testing.T) {
	app, e, posts := slackPostFixture(t, "def main(host): return {}", nil, nil, func(w http.ResponseWriter, n int) {
		w.Write([]byte(`{"ok":true,"channel":"Cposts","ts":"1700000000.00010` + string(rune('0'+n)) + `"}`))
	})
	parent, err := hostStep(t, app, e, "parent", "messages.post", map[string]any{"text": "New ad <!channel> ready", "image": map[string]string{"url": "https://cdn.example.com/ad.png", "alt": "Coffee ad"}})
	if err != nil {
		t.Fatal(err)
	}
	ts := result(t, parent)["ts"]
	if ts != "1700000000.000101" || result(t, parent)["channel"] != "Cposts" {
		t.Fatalf("posted message ID not returned: %v", parent)
	}
	if _, err = hostStep(t, app, e, "inputs", "messages.post", map[string]any{"text": "Inputs: coffee, morning", "thread": ts}); err != nil {
		t.Fatal(err)
	}
	if posts.count() != 2 {
		t.Fatalf("posted %d messages", posts.count())
	}
	first, reply := posts.bodies[0], posts.bodies[1]
	if first["channel"] != "Cposts" || first["thread_ts"] != nil || first["parse"] != "none" || first["unfurl_links"] != false || first["unfurl_media"] != false {
		t.Fatalf("parent post fields: %v", first)
	}
	if text, _ := first["text"].(string); strings.Contains(text, "<!channel>") {
		t.Fatalf("package text can notify the whole channel: %q", text)
	}
	blocks, _ := json.Marshal(first["blocks"])
	if !strings.Contains(string(blocks), `"type":"image"`) || !strings.Contains(string(blocks), `"image_url":"https://cdn.example.com/ad.png"`) || !strings.Contains(string(blocks), `"alt_text":"Coffee ad"`) {
		t.Fatalf("image block missing: %s", blocks)
	}
	if reply["thread_ts"] != ts || reply["channel"] != "Cposts" {
		t.Fatalf("reply was not threaded: %v", reply)
	}
	// Replaying a completed step returns its retained ID without posting again.
	again, err := hostStep(t, app, e, "parent", "messages.post", map[string]any{"text": "New ad <!channel> ready", "image": map[string]string{"url": "https://cdn.example.com/ad.png", "alt": "Coffee ad"}})
	if err != nil || result(t, again)["ts"] != ts || posts.count() != 2 {
		t.Fatalf("replay posted again or lost the ID: %v %v", again, err)
	}
}

func TestScriptPostsSeveralImagesInOneMessage(t *testing.T) {
	app, e, posts := slackPostFixture(t, "def main(host): return {}", nil, nil, func(w http.ResponseWriter, n int) {
		w.Write([]byte(`{"ok":true,"channel":"Cposts","ts":"1700000000.000100"}`))
	})
	first := map[string]string{"url": "https://cdn.example.com/a.png", "alt": "Bold style"}
	second := map[string]string{"url": "https://cdn.example.com/b.png", "alt": "Photo style"}
	if _, err := hostStep(t, app, e, "both", "messages.post", map[string]any{"text": "Two picks", "images": []map[string]string{first, second}}); err != nil {
		t.Fatal(err)
	}
	if posts.count() != 1 {
		t.Fatalf("posted %d messages for one post", posts.count())
	}
	blocks, _ := json.Marshal(posts.bodies[0]["blocks"])
	a, b := strings.Index(string(blocks), `"image_url":"https://cdn.example.com/a.png"`), strings.Index(string(blocks), `"image_url":"https://cdn.example.com/b.png"`)
	if a < 0 || b < a || !strings.Contains(string(blocks), `"alt_text":"Photo style"`) {
		t.Fatalf("both images were not shown in order: %s", blocks)
	}
	five := []map[string]string{first, second, first, second, first}
	for name, post := range map[string]map[string]any{
		"image-and-images": {"text": "Ad", "image": first, "images": []map[string]string{second}},
		"too-many":         {"text": "Ad", "images": five},
		"insecure":         {"text": "Ad", "images": []map[string]string{first, {"url": "http://cdn.example.com/c.png", "alt": "Ad"}}},
		"no-alt":           {"text": "Ad", "images": []map[string]string{{"url": "https://cdn.example.com/c.png"}}},
	} {
		if _, err := hostStep(t, app, e, name, "messages.post", post); err == nil {
			t.Errorf("%s: accepted an invalid image list", name)
		}
	}
	if posts.count() != 1 || e.run.Script.Uncertain() {
		t.Fatalf("refused posts reached Slack (%d) or blocked the job", posts.count())
	}
}

func TestScriptPostsCarouselWithPrivatelyUploadedCardImages(t *testing.T) {
	app, e, posts := slackPostFixture(t, "def main(host): return {}", nil, nil, func(w http.ResponseWriter, n int) {
		w.Write([]byte(`{"ok":true,"channel":"Cposts","ts":"1700000000.000100"}`))
	})
	var picture bytes.Buffer
	if err := jpeg.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 8, 6)), nil); err != nil {
		t.Fatal(err)
	}
	padded := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(picture.Bytes())
	cards := []map[string]any{
		{"title": "Source", "subtitle": "1400×400", "image": map[string]string{"data": padded, "alt": "Coffee ad, source"}},
		{"title": "1200×300 <!channel>", "subtitle": "passed 1st try", "body": "$0.06 · 3 boards", "image": map[string]string{"url": "https://cdn.example.com/r.png", "alt": "Coffee ad, 1200×300"}},
		{"title": "300×900", "subtitle": "needs review"},
	}
	if _, err := hostStep(t, app, e, "carousel", "messages.post", map[string]any{"text": "*AI resize* · Coffee", "cards": cards}); err != nil {
		t.Fatal(err)
	}
	if posts.count() != 1 || len(posts.uploads) != 1 || posts.uploads[0] != picture.String() || len(posts.completes) != 1 {
		t.Fatalf("posts %d, uploads %d, completions %d", posts.count(), len(posts.uploads), len(posts.completes))
	}
	if _, shared := posts.completes[0]["channel_id"]; shared {
		t.Fatalf("card image was shared to a channel as a file: %v", posts.completes[0])
	}
	var blocks []struct {
		Type     string `json:"type"`
		Elements []struct {
			Type      string            `json:"type"`
			Title     map[string]string `json:"title"`
			Subtitle  map[string]string `json:"subtitle"`
			Body      map[string]string `json:"body"`
			HeroImage *struct {
				ImageURL  string            `json:"image_url"`
				SlackFile map[string]string `json:"slack_file"`
				AltText   string            `json:"alt_text"`
			} `json:"hero_image"`
		} `json:"elements"`
	}
	raw, _ := json.Marshal(posts.bodies[0]["blocks"])
	if err := json.Unmarshal(raw, &blocks); err != nil || len(blocks) != 2 || blocks[0].Type != "markdown" || blocks[1].Type != "carousel" || len(blocks[1].Elements) != 3 {
		t.Fatalf("expected the text then one carousel: %s", raw)
	}
	source, resized, review := blocks[1].Elements[0], blocks[1].Elements[1], blocks[1].Elements[2]
	if source.Type != "card" || source.Title["text"] != "Source" || source.HeroImage == nil || source.HeroImage.SlackFile["id"] != "Fcard0" || source.HeroImage.ImageURL != "" || source.HeroImage.AltText != "Coffee ad, source" {
		t.Fatalf("uploaded card image not shown by its Slack file: %s", raw)
	}
	if resized.HeroImage == nil || resized.HeroImage.ImageURL != "https://cdn.example.com/r.png" || resized.Body["text"] != "$0.06 · 3 boards" || resized.Title["type"] != "plain_text" {
		t.Fatalf("URL card wrong: %s", raw)
	}
	if review.HeroImage != nil || review.Subtitle["text"] != "needs review" {
		t.Fatalf("card without an image wrong: %s", raw)
	}

	long := strings.Repeat("x", 151)
	eleven := []map[string]any{}
	for range 11 {
		eleven = append(eleven, map[string]any{"title": "Size"})
	}
	for name, post := range map[string]map[string]any{
		"too-many":     {"text": "Ad", "cards": eleven},
		"with-images":  {"text": "Ad", "cards": cards[:1], "images": []map[string]string{{"url": "https://cdn.example.com/a.png", "alt": "Ad"}}},
		"long-title":   {"text": "Ad", "cards": []map[string]any{{"title": long}}},
		"long-body":    {"text": "Ad", "cards": []map[string]any{{"title": "Size", "body": long + long}}},
		"empty-card":   {"text": "Ad", "cards": []map[string]any{{"subtitle": "Only a subtitle"}}},
		"url-and-data": {"text": "Ad", "cards": []map[string]any{{"image": map[string]string{"url": "https://cdn.example.com/a.png", "data": padded, "alt": "Ad"}}}},
		"not-image":    {"text": "Ad", "cards": []map[string]any{{"image": map[string]string{"data": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("hello")), "alt": "Ad"}}}},
		"wrong-type":   {"text": "Ad", "cards": []map[string]any{{"image": map[string]string{"data": "data:image/png;base64," + base64.StdEncoding.EncodeToString(picture.Bytes()), "alt": "Ad"}}}},
		"too-large":    {"text": "Ad", "cards": []map[string]any{{"image": map[string]string{"data": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(append(picture.Bytes(), make([]byte, 2<<20)...)), "alt": "Ad"}}}},
		"no-alt":       {"text": "Ad", "cards": []map[string]any{{"image": map[string]string{"data": padded}}}},
		"insecure-url": {"text": "Ad", "cards": []map[string]any{{"image": map[string]string{"url": "http://cdn.example.com/a.png", "alt": "Ad"}}}},
	} {
		if _, err := hostStep(t, app, e, name, "messages.post", post); err == nil {
			t.Errorf("%s: accepted an invalid carousel", name)
		}
	}
	if posts.count() != 1 || len(posts.uploads) != 1 || e.run.Script.Uncertain() {
		t.Fatalf("refused carousels reached Slack (%d posts, %d uploads) or blocked the job", posts.count(), len(posts.uploads))
	}

	e.run.DryRun = true
	if v, err := hostStep(t, app, e, "preview", "messages.post", map[string]any{"text": "Preview", "cards": cards}); err != nil || result(t, v)["posted"] != false || len(posts.uploads) != 1 {
		t.Fatalf("preview uploaded or posted: %v %v", v, err)
	}
}

func TestScriptSlackPostRequiresApprovedAudience(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"revision":"one"}`))
	}))
	defer upstream.Close()
	app, e, posts := slackPostFixture(t, "def main(host): return {}", []string{"connectors.read"}, map[string][]string{"api": {"read"}}, func(w http.ResponseWriter, n int) {
		w.Write([]byte(`{"ok":true,"channel":"Cposts","ts":"1700000000.000100"}`))
	})
	bindScriptConnector(t, app, e, upstream.URL)
	if _, err := hostStep(t, app, e, "read", "connectors.read", map[string]any{"connection": "api", "operation": "read", "arguments": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	post := map[string]any{"text": "New ad"}
	// The broadcast approves "warehouse" data, not what this run just read.
	if _, err := hostStep(t, app, e, "unapproved-source", "messages.post", post); err == nil {
		t.Fatal("data from a connection outside the approved audience was posted")
	}
	approved := map[string]SlackBroadcast{"Cposts": {Project: "default", Actors: []string{e.run.Spec.Actor}, Connections: []string{"fixture-api"}, AudienceApproved: true}}
	app.slack.config.Broadcasts = map[string]SlackBroadcast{"Cposts": {Project: "default", Actors: []string{"someone-else"}, Connections: []string{"fixture-api"}, AudienceApproved: true}}
	if _, err := hostStep(t, app, e, "other-actor", "messages.post", post); err == nil {
		t.Fatal("posted for a service account the broadcast does not allow")
	}
	app.slack.config.Broadcasts = nil
	if _, err := hostStep(t, app, e, "revoked", "messages.post", post); err == nil {
		t.Fatal("posted after the channel's broadcast was removed")
	}
	app.slack.config.Broadcasts = approved
	if _, err := hostStep(t, app, e, "bad-thread", "messages.post", map[string]any{"text": "Reply", "thread": "not-a-ts"}); err == nil {
		t.Fatal("accepted an invalid thread ID")
	}
	if _, err := hostStep(t, app, e, "bad-image", "messages.post", map[string]any{"text": "Ad", "image": map[string]string{"url": "http://cdn.example.com/ad.png", "alt": "Ad"}}); err == nil {
		t.Fatal("accepted a non-HTTPS image URL")
	}
	if posts.count() != 0 || e.run.Script.Uncertain() {
		t.Fatalf("refused posts reached Slack (%d) or blocked the job", posts.count())
	}
	if _, err := hostStep(t, app, e, "approved", "messages.post", post); err != nil || posts.count() != 1 {
		t.Fatal("approved post failed", err)
	}
}

func TestScriptSlackRejectionSettlesButUnknownOutcomeBlocks(t *testing.T) {
	app, e, posts := slackPostFixture(t, "def main(host): return {}", nil, nil, func(w http.ResponseWriter, n int) {
		switch n {
		case 1:
			w.Write([]byte(`{"ok":false,"error":"invalid_blocks","errors":["downloading image failed [json-pointer:/blocks/1/image_url]"],"response_metadata":{"messages":["[ERROR] downloading image failed [json-pointer:/blocks/1/image_url]"]}}`))
		case 2:
			w.Write([]byte(`{"ok":true,"channel":"Cposts","ts":"1700000000.000200"}`))
		default:
			// The connection drops after Slack received the request.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		}
	})
	if _, err := hostStep(t, app, e, "rejected", "messages.post", map[string]any{"text": "Ad", "image": map[string]string{"url": "https://cdn.example.com/missing.png", "alt": "Ad"}}); err == nil || !strings.Contains(err.Error(), "invalid_blocks") || !strings.Contains(err.Error(), "downloading image failed [json-pointer:/blocks/1/image_url]") {
		t.Fatal("Slack rejection and its reason were not reported", err)
	}
	if e.run.Script.Uncertain() {
		t.Fatal("a definitive Slack rejection blocked the job")
	}
	if _, err := hostStep(t, app, e, "text-only", "messages.post", map[string]any{"text": "Ad (image unavailable)"}); err != nil {
		t.Fatal(err)
	}
	if _, err := hostStep(t, app, e, "dropped", "messages.post", map[string]any{"text": "Sequence"}); err == nil {
		t.Fatal("unknown outcome reported as success")
	}
	if !e.run.Script.Uncertain() {
		t.Fatal("unknown Slack outcome did not require reconciliation")
	}
	if _, err := hostStep(t, app, e, "dropped", "messages.post", map[string]any{"text": "Sequence"}); err == nil || posts.count() != 3 {
		t.Fatal("unknown outcome was resent", err, posts.count())
	}
	if _, err := hostStep(t, app, e, "next", "messages.post", map[string]any{"text": "Another"}); err == nil || posts.count() != 3 {
		t.Fatal("posted while an earlier outcome is unknown", err, posts.count())
	}
}

func TestScriptSlackPostPreviewSendsNothing(t *testing.T) {
	app, e, posts := slackPostFixture(t, "def main(host): return {}", nil, nil, func(w http.ResponseWriter, n int) {
		w.Write([]byte(`{"ok":true,"channel":"Cposts","ts":"1700000000.000100"}`))
	})
	e.run.DryRun = true
	v, err := hostStep(t, app, e, "preview", "messages.post", map[string]any{"text": "New ad"})
	if err != nil {
		t.Fatal(err)
	}
	if got := result(t, v); got["dryRun"] != true || got["posted"] != false || got["ts"] != nil || posts.count() != 0 {
		t.Fatalf("preview posted or invented an ID: %v (%d posts)", got, posts.count())
	}
}

func TestScriptPackageThreadsRepliesUnderItsOwnPost(t *testing.T) {
	code := `def main(host):
    parent = host.call('parent', 'messages.post', text='*New ad* Morning coffee')
    reply = host.call('inputs', 'messages.post', text='Inputs: coffee', thread=parent['ts'])
    return {'parent': parent['ts'], 'reply': reply['ts']}
`
	app, e, posts := slackPostFixture(t, code, nil, nil, func(w http.ResponseWriter, n int) {
		w.Write([]byte(`{"ok":true,"channel":"Cposts","ts":"1700000000.00010` + string(rune('0'+n)) + `"}`))
	})
	e.run.Script = nil
	if err := app.automationrun.RunScript(e.ctx, e.run, e.save); err != nil {
		t.Fatal(err)
	}
	if posts.count() != 2 || posts.bodies[0]["thread_ts"] != nil || posts.bodies[1]["thread_ts"] != "1700000000.000101" {
		t.Fatalf("reply was not threaded under the package's post: %v", posts.bodies)
	}
	if e.run.Result != `{"parent":"1700000000.000101","reply":"1700000000.000102"}` || e.run.DeliveryState != "" {
		t.Fatalf("package result %s, end-of-run delivery %q", e.run.Result, e.run.DeliveryState)
	}
}
