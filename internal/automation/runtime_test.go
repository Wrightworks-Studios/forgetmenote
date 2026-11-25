package automation

import (
	"context"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeIndependentAIJobsOverlapAndSameJobWaits(t *testing.T) {
	t.Setenv("FMN_LIMIT_AUTOMATION_AI_WORKERS", "2")
	r, err := OpenRuntime(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 4)
	release := make(chan struct{})
	for _, id := range []string{"chat-one", "moderation"} {
		spec := Spec{ID: id, Kind: "assistant", Actor: "robot", Project: "default", Instruction: "Check", ExternalProcessing: true, Delivery: &Delivery{Client: "slack", Channel: "D123", User: "U123", Direct: true}}
		for _, key := range []string{"one", "two"} {
			if _, err := r.Queue.Enqueue(spec, key); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.Start(t.Context(), Hooks{Personal: func() []Spec { return nil }, Policy: func(Spec) error { return nil }, Run: func(ctx context.Context, run *Run, _ func() error) error {
		started <- run.Spec.ID
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	t.Cleanup(r.Close)
	seen := map[string]bool{}
	for range 2 {
		select {
		case id := <-started:
			if seen[id] {
				t.Fatal("same job ran concurrently")
			}
			seen[id] = true
		case <-time.After(3 * time.Second):
			t.Fatal("independent AI job blocked behind another job")
		}
	}
	select {
	case <-started:
		t.Fatal("worker capacity exceeded")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("queued same-job run never resumed")
		}
	}
}

func TestRuntimeCloseCancelsAndDrainsConcurrentJobs(t *testing.T) {
	t.Setenv("FMN_LIMIT_AUTOMATION_AI_WORKERS", "2")
	r, err := OpenRuntime(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	started, drained := make(chan struct{}, 2), make(chan struct{}, 2)
	for _, id := range []string{"one", "two"} {
		if _, err := r.Queue.Enqueue(Spec{ID: id, Kind: "assistant", Actor: "robot", Project: "default", Instruction: "Check", ExternalProcessing: true, Delivery: &Delivery{Client: "slack", Channel: "D123", User: "U123", Direct: true}}, "one"); err != nil {
			t.Fatal(err)
		}
	}
	r.Start(t.Context(), Hooks{Personal: func() []Spec { return nil }, Policy: func(Spec) error { return nil }, Run: func(ctx context.Context, _ *Run, _ func() error) error {
		started <- struct{}{}
		<-ctx.Done()
		drained <- struct{}{}
		return ctx.Err()
	}})
	t.Cleanup(r.Close)
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("independent job did not start")
		}
	}
	r.Close()
	if len(drained) != 2 {
		t.Fatal("close returned before workers drained")
	}
	runs, err := r.Queue.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.State != "queued" {
			t.Fatalf("cancelled work lost recovery state: %s", run.State)
		}
	}
}

func idleHooks(run func(context.Context, *Run, func() error) error) Hooks {
	return Hooks{Personal: func() []Spec { return nil }, Policy: func(Spec) error { return nil }, Run: run}
}

func TestRuntimeClaimsWorkAnotherGatewayAnnounces(t *testing.T) {
	db := pgtest.Org(t)
	other, _ := pgtest.Gateway(t, db)
	r, err := OpenRuntime(db)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	r.Start(t.Context(), idleHooks(func(context.Context, *Run, func() error) error {
		started <- struct{}{}
		return nil
	}))
	t.Cleanup(r.Close)
	pgtest.Listening(t, db, QueueChannel)
	b, err := Open(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Enqueue(gatewaySpec("one"), "key"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(claimFallback / 2):
		t.Fatal("work queued on another gateway waited for the claimer's fallback scan")
	}
}

func TestRuntimeStartsARetryWhenItsDelayEnds(t *testing.T) {
	r, err := OpenRuntime(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.Queue.Enqueue(gatewaySpec("one"), "key")
	if err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().Add(1500 * time.Millisecond)
	run.RetryAt = retryAt
	if err = r.Queue.write(&run); err != nil {
		t.Fatal(err)
	}
	started := make(chan time.Time, 1)
	r.Start(t.Context(), idleHooks(func(context.Context, *Run, func() error) error {
		started <- time.Now()
		return nil
	}))
	t.Cleanup(r.Close)
	select {
	case at := <-started:
		if at.Before(retryAt) {
			t.Fatal("a retry started before its delay ended")
		}
		if at.Sub(retryAt) > time.Second {
			t.Fatalf("a retry started %s after its delay ended", at.Sub(retryAt))
		}
	case <-time.After(claimFallback):
		t.Fatal("a retry waited for the claimer's fallback scan")
	}
}

func TestRuntimeFindsUnannouncedWorkWithinTheFallback(t *testing.T) {
	r, err := OpenRuntime(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Bool
	started := make(chan struct{}, 1)
	hooks := idleHooks(func(context.Context, *Run, func() error) error {
		started <- struct{}{}
		return nil
	})
	hooks.Policy = func(Spec) error {
		if !allowed.Load() {
			return errors.New("not yet allowed")
		}
		return nil
	}
	r.Start(t.Context(), hooks)
	t.Cleanup(r.Close)
	if _, err = r.Queue.Enqueue(gatewaySpec("one"), "key"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
		t.Fatal("a run the policy refused started")
	case <-time.After(500 * time.Millisecond):
	}
	// Nothing announces a policy change; the fallback scan finds the run.
	allowed.Store(true)
	select {
	case <-started:
	case <-time.After(claimFallback + 2*time.Second):
		t.Fatal("a run the policy now admits was never claimed")
	}
}

func TestRuntimeSchedulesAJobSavedOnAnotherGateway(t *testing.T) {
	db := pgtest.Org(t)
	other, _ := pgtest.Gateway(t, db)
	r, err := OpenRuntime(db)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 1)
	r.Start(t.Context(), idleHooks(func(_ context.Context, run *Run, _ func() error) error {
		started <- run.Spec.ID
		return nil
	}))
	t.Cleanup(r.Close)
	pgtest.Listening(t, db, QueueChannel)
	// Let the scheduler take its lease and read the jobs once.
	time.Sleep(1500 * time.Millisecond)
	definitions, err := OpenDefinitions(other)
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "hourly", Kind: "digest", Actor: "robot", Project: "notes", Output: "report.md", IntervalSeconds: 3600, Calls: []Call{{Connection: "metrics", Operation: "read"}}}
	if _, err = definitions.Save(spec, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-started:
		if id != spec.ID {
			t.Fatal("ran", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a job saved on another gateway waited for the scheduler's fallback")
	}
}

func TestAClaimWhoseContextEndedFirstIsQueuedAgainUnrun(t *testing.T) {
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	queued, err := q.Enqueue(gatewaySpec("one"), "key")
	if err != nil {
		t.Fatal(err)
	}
	// A claim handed to a worker as the runtime closes.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if ran, err := q.RunNext(ctx, func(context.Context, *Run, func() error) error {
		t.Error("work began after its context ended")
		return nil
	}); err != nil || !ran {
		t.Fatal(ran, err)
	}
	got, err := q.Get(queued.ID)
	if err != nil || got.State != "queued" || got.Attempts != 0 || got.Claimant != "" {
		t.Fatalf("unrun claim left %s after %d attempts, claimant %q: %v", got.State, got.Attempts, got.Claimant, err)
	}
}

// Claims weigh a run's whole spec. What a policy reads of it, such as a
// personal binding or an assistant's instruction, is not kept in the scans'
// summaries, and a running personal run stays current across scheduler passes.
func TestRuntimeClaimsRunsByTheirWholeSpec(t *testing.T) {
	r, err := OpenRuntime(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	account := strings.Repeat("a", 32)
	personal := Spec{ID: "personal-" + account, Kind: "personal", Actor: "robot", Project: "assistant-" + account, ExternalProcessing: true, Personal: &Personal{Account: account, Settings: 1, Policy: 1}}
	var runs []Run
	for _, spec := range []Spec{personal, gatewaySpec("chat")} {
		run, err := r.Queue.Enqueue(spec, "key")
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	started := make(chan string, 2)
	hooks := idleHooks(func(ctx context.Context, run *Run, _ func() error) error {
		started <- run.Spec.ID
		// Outlast the scheduler's pass after the claims.
		select {
		case <-time.After(2500 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	hooks.Personal = func() []Spec { return []Spec{personal} }
	// As the web policy checks a personal binding and what an assistant does.
	hooks.Policy = func(spec Spec) error {
		if spec.Actor == "" || spec.Kind == "personal" && spec.Personal == nil || spec.Kind == "assistant" && spec.Instruction == "" {
			return errors.New("incomplete automation spec")
		}
		return nil
	}
	r.Start(t.Context(), hooks)
	t.Cleanup(r.Close)
	for range runs {
		select {
		case <-started:
		case <-time.After(claimFallback / 2):
			t.Fatal("a run whose policy reads its whole spec was not claimed")
		}
	}
	for _, run := range runs {
		deadline := time.Now().Add(5 * time.Second)
		for {
			got, err := r.Queue.Get(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if terminal(got.State) {
				if got.State != "completed" {
					t.Fatalf("%s run ended %s: %s", got.Spec.Kind, got.State, got.Error)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s run still %s", got.Spec.Kind, got.State)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// A script whose AI use comes from its capabilities runs in the AI pool, not
// in the capacity reserved for deterministic work.
func TestRuntimeRunsAScriptThatUsesAIInTheAIPool(t *testing.T) {
	t.Setenv("FMN_LIMIT_AUTOMATION_WORKERS", "1")
	r, err := OpenRuntime(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	var specs []Spec
	for _, id := range []string{"blocker", "thinker"} {
		spec := scriptSpec(t)
		spec.ID, spec.Disabled = id, false
		if id == "thinker" {
			spec.ExternalProcessing = true
			spec.Script.Capabilities = []string{"ai.structured"}
		}
		saved, err := r.Definitions.Save(spec, "")
		if err != nil {
			t.Fatal(err)
		}
		specs = append(specs, saved.Spec)
	}
	started, release := make(chan string, 2), make(chan struct{})
	r.Start(t.Context(), idleHooks(func(ctx context.Context, run *Run, _ func() error) error {
		started <- run.Spec.ID
		if run.Spec.ID == "thinker" {
			return nil
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	t.Cleanup(r.Close)
	t.Cleanup(func() { close(release) })
	for _, spec := range specs {
		if _, err := r.Queue.Enqueue(spec, "key"); err != nil {
			t.Fatal(err)
		}
		select {
		case id := <-started:
			if id != spec.ID {
				t.Fatal("ran", id)
			}
		case <-time.After(claimFallback / 2):
			// The deterministic pool's one worker runs the blocker.
			t.Fatalf("%s did not start", spec.ID)
		}
	}
}
