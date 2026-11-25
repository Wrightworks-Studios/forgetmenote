package main

import (
	"fmt"
	"io"

	"forgetmenote/internal/integrations"
)

// runIntegrations prints each optional integration and whether this binary
// includes it (docs/integrations.md). It reads no instance state.
func runIntegrations(args []string, out io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: forgetmenote integrations")
	}
	for _, integration := range integrations.All() {
		state := "included"
		if !integrations.Included(integration.Name) {
			state = "omitted"
		}
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", integration.Name, state, integration.Title); err != nil {
			return err
		}
	}
	return nil
}
