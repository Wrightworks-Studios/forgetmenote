package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func (c *Client) ConnectGitHub(ctx context.Context, repository, id, name string, noBrowser bool, diagnostics, out io.Writer) error {
	if id == "" || !strings.Contains(repository, "/") {
		return errors.New("provide OWNER/REPO and --id NOTEBOOK")
	}
	if name == "" {
		name = id
	}
	var start struct{ ID, URL string }
	if err := c.Call(ctx, "begin_github_connection", map[string]any{"project": id, "name": name, "purpose": "source", "creating": true}, &start); err != nil {
		return err
	}
	// The URL comes from the server and is handed to the system opener.
	if !strings.HasPrefix(start.URL, "https://github.com/") {
		return errors.New("server returned a GitHub authorization URL outside https://github.com/; refusing to open it")
	}
	fmt.Fprintln(diagnostics, "Authorize the GitHub connection:\n"+start.URL)
	if !noBrowser {
		program := "xdg-open"
		if runtime.GOOS == "darwin" {
			program = "open"
		}
		cmd := exec.CommandContext(ctx, program, start.URL)
		if cmd.Start() == nil {
			go cmd.Wait()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		var status struct {
			Ready   bool
			Choices []struct {
				ID, Installation int64
				FullName         string `json:"full_name"`
			}
		}
		if err := c.Call(ctx, "github_connection_status", map[string]string{"connection": start.ID}, &status); err != nil {
			return err
		}
		if status.Ready {
			for _, choice := range status.Choices {
				if strings.EqualFold(choice.FullName, repository) {
					var result any
					if err := c.Call(ctx, "finish_github_connection", map[string]any{"connection": start.ID, "repository": choice.ID, "installation": choice.Installation, "externalProcessing": false, "shareWithNotebook": true}, &result); err != nil {
						return err
					}
					return printJSON(out, result)
				}
			}
			return errors.New("selected repository is not available to this GitHub installation")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
