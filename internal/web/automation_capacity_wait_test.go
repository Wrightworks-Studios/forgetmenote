package web

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestAutomationWaitsForProviderSlotAndReleasesIt(t *testing.T) {
	app, _ := runnerFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("admission contacted provider") })
	config := app.chat.Config
	connection := config.Connections[0]
	config.Limits.MaxConcurrent = 1
	if _, err := app.chat.Admit(config, connection, "occupied", func() {}, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	type admitted struct {
		release func()
		err     error
	}
	done := make(chan admitted, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		_, release, err := app.reserveAutomationModel(ctx, config, connection)
		done <- admitted{release, err}
	}()
	select {
	case got := <-done:
		if got.release != nil {
			got.release()
		}
		t.Fatalf("did not wait: %v", got.err)
	case <-time.After(150 * time.Millisecond):
	}
	app.chat.Finish("occupied", connection.ID)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		got.release()
	case <-time.After(3 * time.Second):
		t.Fatal("available slot never admitted")
	}
	app.chat.Mu.Lock()
	defer app.chat.Mu.Unlock()
	if len(app.chat.Active) != 0 || app.chat.Connections[connection.ID] != 0 {
		t.Fatal("admission slot leaked")
	}
}

func TestAutomationWaitingSlotStopsOnConfigurationReplacement(t *testing.T) {
	app, _ := runnerFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("stale provider contacted") })
	config := app.chat.Config
	connection := config.Connections[0]
	app.chat.Mu.Lock()
	replacement := *config
	app.chat.Config = &replacement
	app.chat.Mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, release, err := app.reserveAutomationModel(ctx, config, connection)
	if release != nil {
		release()
	}
	if err == nil || ctx.Err() != nil {
		t.Fatal("stale configuration waited or admitted", err)
	}
}
