package chat

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/durable"
	"forgetmenote/internal/sources"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ResearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
}
type Research struct {
	provider             *Provider
	client               *http.Client
	endpoint, credential string
}

// NewResearch binds the one documented Brave contract to operator configuration.
// Tools can provide search text; they cannot supply headers, credentials or endpoints.
func NewResearch(c ResearchConfig) (*Research, error) {
	if !c.Enabled || c.Format != "brave" || validateEndpoint(c.BaseURL, false) != nil {
		return nil, errors.New("invalid research configuration")
	}
	token, err := durable.ReadSecret(c.SecretFile, 16384)
	switch {
	case errors.Is(err, durable.ErrOpen):
		return nil, errors.New("cannot read configured research credential")
	case errors.Is(err, durable.ErrNotRegular):
		return nil, errors.New("invalid research credential file")
	case err != nil:
		return nil, errors.New("invalid research credential")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 8 * time.Second}).DialContext, TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 64 << 10, DisableKeepAlives: true}
	return &Research{endpoint: c.BaseURL, credential: token, client: &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (r *Research) Search(ctx context.Context, query string, count int) ([]ResearchResult, error) {
	if r != nil && r.provider != nil {
		results, _, err := r.SearchUsage(ctx, query, count)
		return results, err
	}
	if r == nil {
		return nil, errors.New("external research is disabled")
	}
	if strings.TrimSpace(query) == "" || len(query) > 2000 || strings.ContainsRune(query, 0) || count < 1 || count > 10 {
		return nil, errors.New("search requires 1–2000 bytes and 1–10 results")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u, _ := url.Parse(r.endpoint)
	q := u.Query()
	q.Set("q", query)
	q.Set("count", strconv.Itoa(count))
	q.Set("result_filter", "web")
	q.Set("text_decorations", "false")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, errors.New("invalid research request")
	}
	req.Header.Set("X-Subscription-Token", r.credential)
	req.Header.Set("Accept", "application/json")
	response, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("configured research search is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, errors.New("configured research search rejected the request")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		return nil, errors.New("research response exceeded limits or was interrupted")
	}
	var body struct {
		Web struct {
			Results []ResearchResult `json:"results"`
		} `json:"web"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil, errors.New("invalid research response")
	}
	out := []ResearchResult{}
	seen := map[string]bool{}
	for _, v := range body.Web.Results {
		if sources.ValidateURL(v.URL) != nil || seen[v.URL] || len(v.Title) > 1000 || len(v.Description) > 8000 {
			continue
		}
		seen[v.URL] = true
		out = append(out, v)
		if len(out) == count {
			break
		}
	}
	return out, nil
}
