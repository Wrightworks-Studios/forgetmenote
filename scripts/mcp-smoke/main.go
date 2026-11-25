// A protocol smoke client for disposable projects. It creates one synthetic note
// only with -write, then records its own output checkpoint to avoid a trigger loop.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	endpoint := flag.String("endpoint", "", "MCP URL")
	file := flag.String("token-file", "", "private bearer token file")
	project := flag.String("project", "", "disposable project")
	write := flag.Bool("write", false, "create one synthetic note and record a test checkpoint")
	legacy := flag.Bool("legacy", false, "exercise legacy 2025-11-25 negotiation")
	flag.Parse()
	if *endpoint == "" || *file == "" || *project == "" {
		return errors.New("supply -endpoint, -token-file and -project; -write is only for disposable data")
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	updates := make(chan string, 1)
	ack := make(chan struct{}, 1)
	client := mcp.NewClient(&mcp.Implementation{Name: "forgetmenote-protocol-smoke", Version: "1"}, &mcp.ClientOptions{ResourceUpdatedHandler: func(ctx context.Context, r *mcp.ResourceUpdatedNotificationRequest) {
		select {
		case updates <- r.Params.URI:
		default:
		}
	}})
	client.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
			if method == "notifications/subscriptions/acknowledged" {
				select {
				case ack <- struct{}{}:
				default:
				}
			}
			return next(ctx, method, r)
		}
	})
	if *legacy {
		client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
				if method == "server/discover" {
					return nil, &jsonrpc.Error{Code: -32601, Message: "legacy fixture"}
				}
				return next(ctx, method, r)
			}
		})
	}
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: *endpoint, HTTPClient: &http.Client{Transport: bearer{strings.TrimSpace(string(raw))}}}, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	call := func(name string, args map[string]any) (map[string]any, error) {
		args["project"] = *project
		r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return nil, err
		}
		if r.IsError {
			raw, _ := json.Marshal(r.Content)
			return nil, fmt.Errorf("%s rejected: %s", name, raw)
		}
		raw, err := json.Marshal(r.StructuredContent)
		if err != nil {
			return nil, err
		}
		var out map[string]any
		err = json.Unmarshal(raw, &out)
		return out, err
	}
	if _, err = call("project_instructions", map[string]any{}); err != nil {
		return err
	}
	uri := "forgetmenote://projects/" + *project + "/revision"
	if _, err = session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri}); err != nil {
		return err
	}
	if err = session.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
		return err
	}
	defer session.Unsubscribe(context.Background(), &mcp.UnsubscribeParams{URI: uri})
	if session.InitializeResult().ProtocolVersion >= "2026-07-28" {
		select {
		case <-ack:
		case <-ctx.Done():
			return errors.New("subscription was not acknowledged")
		}
	}
	fmt.Println("Subscribed using", session.InitializeResult().ProtocolVersion)
	if !*write {
		return nil
	}
	id := fmt.Sprintf("smoke-%d", time.Now().UnixNano())
	saved, err := call("submit_change_group", map[string]any{"mode": "apply", "group": map[string]any{"id": id, "summary": "Synthetic notification verification", "edits": []map[string]any{{"path": "notes/" + id + ".md", "content": "# Protocol fixture\nSynthetic disposable content.\n", "revision": "", "hash": "", "delete": false}}}})
	if err != nil {
		return err
	}
	select {
	case got := <-updates:
		if got != uri {
			return errors.New("unexpected resource URI")
		}
	case <-ctx.Done():
		return errors.New("no notification through the proxy")
	}
	revision, _ := saved["revision"].(string)
	if revision == "" {
		return fmt.Errorf("missing group revision: %v", saved)
	}
	if _, err = call("complete_workflow", map[string]any{"completion": map[string]any{"consumer": id, "previous": "", "revision": revision, "summary": "Synthetic output successfully read", "reset": false}}); err != nil {
		return err
	}
	checkpoint, err := call("consumer_checkpoint", map[string]any{"consumer": id})
	if err != nil {
		return err
	}
	changes, _ := checkpoint["changes"].(map[string]any)
	files, _ := changes["files"].([]any)
	if len(files) != 0 || changes["freshReview"] != false {
		return errors.New("consumer would reprocess its own completed output")
	}
	select {
	case <-updates:
		return errors.New("checkpoint generated a content notification")
	case <-time.After(300 * time.Millisecond):
	}
	fmt.Println("PASS: committed group notification, authorized catch-up, idempotent consumer boundary and no checkpoint-trigger loop")
	return nil
}
