package messaging

import "strings"

// StopRequested recognizes direct conversation controls, not quoted instructions
// or incidental uses of "stop" in a task. Other channel messages require mentions.
func StopRequested(text string) bool {
	text = strings.TrimSpace(text)
	for strings.HasPrefix(text, "<@") {
		_, rest, ok := strings.Cut(text, ">")
		if !ok {
			return false
		}
		text = strings.TrimSpace(rest)
	}
	text = strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(text, "’", "'"))), " ")
	text = strings.Trim(text, ".!? ")
	for _, prefix := range []string{"you can ", "could you ", "can you ", "would you ", "please ", "just "} {
		text = strings.TrimPrefix(text, prefix)
	}
	text = strings.TrimPrefix(text, "please ")
	for _, suffix := range []string{" to this thread", " in this thread", " for now", " here", " now"} {
		text = strings.TrimSuffix(text, suffix)
	}
	switch text {
	case "stop", "stop responding", "stop replying", "stop answering", "stop here", "don't respond", "do not respond", "don't reply", "do not reply":
		return true
	}
	return false
}
