//go:build fmn_omit_slack

package messaging

import "context"

// RememberMembers returns ctx: without the Slack transport there are no
// member answers to reuse.
func RememberMembers(ctx context.Context) context.Context { return ctx }
