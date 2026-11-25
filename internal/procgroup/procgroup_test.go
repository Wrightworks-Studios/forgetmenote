package procgroup

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommandCancellationKillsTheWholeGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := Command(ctx, "sh", "-c", "sleep 60 & echo $!; wait")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(pipe).ReadString('\n')
	child, convErr := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || convErr != nil {
		t.Fatal("child PID:", line, err, convErr)
	}
	started := time.Now()
	cancel()
	if err = cmd.Wait(); err == nil {
		t.Fatal("canceled command succeeded")
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("cancellation waited for the child")
	}
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(child, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("grandchild survived cancellation")
		}
	}
	if err = cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatal("canceling an exited group:", err)
	}
}

func TestOutputRefusesWritesPastItsLimit(t *testing.T) {
	out := Output{Limit: 4}
	if n, err := out.Write([]byte("abcd")); n != 4 || err != nil || out.Exceeded {
		t.Fatal("write within the limit:", n, err)
	}
	if n, err := out.Write([]byte("e")); n != 0 || !errors.Is(err, ErrLimit) || !out.Exceeded || out.String() != "abcd" {
		t.Fatal("write past the limit:", n, err, out.String())
	}
	custom := errors.New("budget")
	capped := Output{Limit: 2, Err: custom}
	if _, err := capped.Write([]byte("abc")); err != custom || capped.Len() != 0 {
		t.Fatal("custom limit error:", err, capped.Len())
	}
}
