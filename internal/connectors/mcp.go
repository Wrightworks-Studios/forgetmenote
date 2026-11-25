package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type credentialTransport struct {
	runtime *runtime
	secret  string
}

func (t credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	endpoint := t.runtime.config.Endpoint
	// Credentials are only attached to the configured origin, including any
	// subsequent transport requests. Remote resource links are never fetched here.
	if req.URL.String() != endpoint {
		return nil, ErrInvalid
	}
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	t.runtime.authorize(clone, t.secret)
	if err := checkAuthority(req.Context()); err != nil {
		return nil, err
	}
	resp, err := t.runtime.client.Transport.RoundTrip(clone)
	if err == nil {
		resp.Body = &limitedBody{ReadCloser: resp.Body, left: int64(t.runtime.config.MaxBytes) + 1}
	}
	return resp, err
}

type limitedBody struct {
	io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errors.New("upstream response exceeds connection limit")
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, e := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, e
}
func (r *runtime) session(ctx context.Context, secret string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "forgetmenote", Version: "1"}, nil)
	httpClient := noRedirectClient(credentialTransport{r, secret}, r.client.Timeout)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: r.config.Endpoint, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	return session, nil
}
func (r *runtime) sessionTools(ctx context.Context, session *mcp.ClientSession) ([]Operation, error) {
	allowed := map[string]Operation{}
	for _, op := range r.config.Operations {
		allowed[op.Name] = op
	}
	out := []Operation{}
	seen := map[string]bool{}
	count := 0
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, ErrUnavailable
		}
		count++
		if count > 4096 {
			return nil, errors.New("upstream tool inventory exceeds limit")
		}
		op, ok := allowed[tool.Name]
		if !ok {
			continue
		}
		if seen[tool.Name] {
			return nil, ErrInvalid
		}
		seen[tool.Name] = true
		var err error
		op.InputSchema, err = json.Marshal(tool.InputSchema)
		if err != nil {
			return nil, ErrInvalid
		}
		if tool.OutputSchema != nil {
			op.OutputSchema, err = json.Marshal(tool.OutputSchema)
			if err != nil {
				return nil, ErrInvalid
			}
		}
		if op.Description == "" {
			op.Description = tool.Description
		}
		out = append(out, op)
	}
	return out, nil
}
func mcpCall(ctx context.Context, session *mcp.ClientSession, name string, input map[string]any) (any, error) {
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: input})
	if err != nil || result.IsError {
		return nil, errors.New("upstream MCP operation failed")
	}
	if result.StructuredContent != nil {
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return nil, ErrInvalid
		}
		var value any
		err = json.Unmarshal(raw, &value)
		return value, err
	}
	return map[string]any{"content": result.Content}, nil
}
