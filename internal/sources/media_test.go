package sources

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMediaOriginalNeedsNoHostedConsent(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("requires FFmpeg")
	}
	raw, err := os.ReadFile("testdata/announcement.wav")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Prepare(t.Context(), "speech.wav", "", raw)
	if err != nil || got.Record.Extraction != "no-text" || !strings.Contains(got.Record.Warning, "without transcription") || string(got.Original) != string(raw) {
		t.Fatalf("local media original import: %q %s %v", got.Text, got.Record.Warning, err)
	}
}
func TestHostedTranscriptionRetainsAllMediaFormats(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("requires FFmpeg")
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/audio/transcriptions" {
			t.Error("wrong transcription endpoint", r.URL.Path)
		}
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Error(err)
			http.Error(w, "bad multipart", 400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("model") != "synthetic-transcribe" {
			t.Error("unconfigured transcription model")
		}
		json.NewEncoder(w).Encode(map[string]any{"text": "Amber speech and Cobalt evidence.", "model": "synthetic-transcribe", "usage": map[string]int{"input_tokens": 20, "output_tokens": 8}})
	}))
	defer server.Close()
	ledger, err := decision.OpenLedger(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	for _, ext := range []string{"wav", "mp3", "m4a", "aac", "ogg", "flac", "mp4", "mov", "avi"} {
		t.Run(ext, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "speech."+ext)
			cmd := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-i", "testdata/announcement.wav", file)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("encode %s: %s %v", ext, output, err)
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			hosted, err := NewHosted(HostedConfig{Endpoint: server.URL, TranscriptionModel: "synthetic-transcribe", Local: true, ContextTokens: 32768, OutputTokens: 2048}, ledger, decision.Scope{Actor: "synthetic", Notebook: "notes", Feature: "extraction", Run: ext}, func(context.Context) error { return nil }, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			got, err := PrepareWithOptions(t.Context(), "speech."+ext, "", raw, Options{Transcribe: true, hosted: hosted})
			if err != nil || !strings.Contains(got.Text, "Amber speech") || !strings.Contains(got.Text, "## Segment 1") || string(got.Original) != string(raw) {
				t.Fatalf("hosted %s: %q %v", ext, got.Text, err)
			}
		})
	}
	if calls.Load() != 9 {
		t.Fatal("duplicate or missing transcription requests", calls.Load())
	}
}

func TestHostedMediaChunksAndDurationBudget(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("requires FFmpeg")
	}
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Error(err)
			http.Error(w, "bad upload", 400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		json.NewEncoder(w).Encode(map[string]any{"text": "Amber segment evidence"})
	}))
	defer provider.Close()
	ledger, err := decision.OpenLedger(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	for _, seconds := range []string{"31", "301"} {
		filename := filepath.Join(t.TempDir(), "long.wav")
		if out, err := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono", "-t", seconds, filename).CombinedOutput(); err != nil {
			t.Fatalf("generate duration fixture: %s %v", out, err)
		}
		raw, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		hosted, err := NewHosted(HostedConfig{Endpoint: provider.URL, TranscriptionModel: "synthetic-transcribe", Local: true, ContextTokens: 32768, OutputTokens: 2048}, ledger, decision.Scope{Actor: "synthetic", Notebook: "notes", Feature: "extraction", Run: seconds}, func(context.Context) error { return nil }, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		got, err := PrepareWithOptions(t.Context(), "long.wav", "", raw, Options{Transcribe: true, hosted: hosted})
		if seconds == "31" {
			if err != nil || strings.Count(got.Text, "Amber segment evidence") != 2 || !strings.Contains(got.Text, "0:30–0:31") {
				t.Fatalf("chunk boundaries: %q %v", got.Text, err)
			}
		} else if !errors.Is(err, ErrRejected) {
			t.Fatalf("recording longer than five minutes: %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("duration budget or chunk count", calls.Load())
	}
}
