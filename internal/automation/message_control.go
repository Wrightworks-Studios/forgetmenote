package automation

import (
	"math/big"
	"regexp"

	"forgetmenote/internal/messaging"
)

var slackTimestamp = regexp.MustCompile(`^[0-9]{1,20}\.[0-9]{1,20}$`)

func slackStop(r Run) bool {
	return !r.DryRun && r.Spec.Kind == "assistant" && r.Spec.Delivery != nil && r.Spec.Delivery.Client == "slack" && messaging.StopRequested(r.Spec.Instruction)
}

func sameMessageThread(a, b Run) bool {
	x, y := a.Spec.Delivery, b.Spec.Delivery
	return !a.DryRun && !b.DryRun && a.Spec.Kind == "assistant" && b.Spec.Kind == "assistant" && x != nil && y != nil && x.Client == "slack" && y.Client == x.Client && x.Workspace == y.Workspace && x.Channel == y.Channel && x.Thread == y.Thread && x.Direct == y.Direct && x.Shared == y.Shared && (x.Shared || x.User == y.User)
}

func messageBefore(a, b Run) bool {
	// Slack event delivery and retries need not arrive in message order. Compare
	// decimal timestamps exactly, including boundaries between epoch seconds.
	x, y := a.Spec.Delivery.Message, b.Spec.Delivery.Message
	if slackTimestamp.MatchString(x) && slackTimestamp.MatchString(y) {
		left, ok := new(big.Rat).SetString(x)
		right, valid := new(big.Rat).SetString(y)
		if ok && valid {
			return left.Cmp(right) <= 0
		}
	}
	return !a.Created.After(b.Created)
}

// Stop controls are ordinary retained, cancelled run records. Their existing
// immutable input fences delayed deliveries and survives restart without a
// second conversation-state store. Explicit later mentions resume normally.
func (q *Queue) messageStopped(r Run, runs []Run) bool {
	if r.Spec.Delivery == nil || r.Spec.Delivery.Client != "slack" || r.Spec.Kind != "assistant" {
		return false
	}
	for _, prior := range runs {
		if q.summaries[prior.ID].stopMessage && sameMessageThread(r, prior) && messageBefore(r, prior) {
			return true
		}
	}
	return false
}

// Caller holds the queue lock, so no worker can claim between the durable stop
// and cancellation. Retrying a stop completes an interrupted cancellation pass.
func (q *Queue) cancelMessageRuns(stop Run, runs []Run) error {
	for _, prior := range runs {
		if prior.ID != stop.ID && !terminal(prior.State) && sameMessageThread(prior, stop) && messageBefore(prior, stop) {
			if err := q.cancel(prior.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
