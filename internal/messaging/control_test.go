package messaging

import "testing"

func TestStopRequested(t *testing.T) {
	for _, text := range []string{"stop", "<@UBOT> you can stop responding to this thread", "<@UBOT> Please stop replying here.", "Could you please stop responding in this thread?", "Don't reply", "do not respond for now"} {
		if !StopRequested(text) {
			t.Errorf("stop command not recognized: %q", text)
		}
	}
	for _, text := range []string{"Don't stop responding", "Please explain how to stop a campaign", "The client said stop responding", "What does 'stop responding' mean?", "stop the campaign", "stop by the office"} {
		if StopRequested(text) {
			t.Errorf("task text mistaken for a stop: %q", text)
		}
	}
}
