package web

import (
	"bufio"
	"fmt"
	"forgetmenote/internal/catalog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stalledClientConn is the server side of a client that stops reading once a
// response has started. The status line and headers reach the client; later
// writes block until release, as against a full TCP window. Real socket buffers
// are platform-tuned up to megabytes, and rendering a note that large under the
// race detector consumed most of the client's read deadline.
type stalledClientConn struct {
	net.Conn
	written int
	release <-chan struct{}
}

const stalledClientReceives = 16 << 10

func (c *stalledClientConn) Write(p []byte) (int, error) {
	n := 0
	if room := stalledClientReceives - c.written; room > 0 {
		var err error
		n, err = c.Conn.Write(p[:min(len(p), room)])
		c.written += n
		if err != nil || n == len(p) {
			return n, err
		}
	}
	<-c.release
	return n, net.ErrClosed
}

type stalledClientListener struct {
	net.Listener
	release <-chan struct{}
}

func (l stalledClientListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &stalledClientConn{Conn: conn, release: l.release}, nil
}

func TestAPIReadAdmissionCoversBlockedResponse(t *testing.T) {
	t.Setenv("FMN_LIMIT_DECODE_REQUESTS", "1")
	t.Setenv("FMN_LIMIT_READ_REQUESTS", "1")
	t.Setenv("FMN_LIMIT_WRITE_REQUESTS", "1")
	app, store := fixture(t)
	// The response carries this note and its HTML, many times what the client receives.
	if _, err := store.Save(t.Context(), "notes/large.md", []byte(strings.Repeat("large notebook text ", 10000)), "", catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/actions/read_note" {
			defer close(finished)
		}
		app.ServeHTTP(w, r)
	})
	release := make(chan struct{})
	server := httptest.NewUnstartedServer(handler)
	server.Listener = stalledClientListener{Listener: server.Listener, release: release}
	server.Start()
	defer server.Close()
	defer close(release)
	app.origin = server.URL
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"project":"default","path":"notes/large.md"}`
	fmt.Fprintf(conn, "POST /api/v1/actions/read_note HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", strings.TrimPrefix(server.URL, "http://"), testToken, len(body), body)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("large read failed: status=%q err=%v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	// The client stops reading the response while its socket remains open.
	select {
	case <-finished:
		t.Fatal("fixture response already finished; expected a blocked response write")
	default:
	}
	out := apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, testToken)
	if out.Code != 429 {
		t.Fatalf("ongoing large response owns no read capacity: second read status=%d", out.Code)
	}
	out = apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"notes/while-output.md","content":"# Save","revision":""}`, testToken)
	if out.Code != 200 {
		t.Fatalf("held read-response capacity starved write: %d %s", out.Code, out.Body.String())
	}
}
