package jsonfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestOperatorJSONBoundsAndStrictValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.json")
	for _, test := range []struct {
		raw      string
		maximum  int64
		accepted bool
	}{
		{`{"enabled":true}`, 16, true},
		{`{"enabled":true}`, 15, false},
		{`{"enabled":true} {}`, 64, false},
		{`{"enabled":true,"unknown":1}`, 64, false},
		{`{"enabled":`, 64, false},
	} {
		if err := os.WriteFile(path, []byte(test.raw), 0600); err != nil {
			t.Fatal(err)
		}
		var value struct {
			Enabled bool `json:"enabled"`
		}
		err := Load(path, test.maximum, &value)
		if (err == nil) != test.accepted {
			t.Fatalf("bounded strict configuration: length=%d limit=%d error=%v", len(test.raw), test.maximum, err)
		}
		if err == nil && !value.Enabled {
			t.Fatal("configuration lost")
		}
	}
	if err := Load(filepath.Dir(path), 64, new(any)); err == nil {
		t.Fatal("directory accepted as configuration")
	}
}

func TestDecodeKeepsReaderFailures(t *testing.T) {
	failure := errors.New("synthetic body limit")
	for _, reader := range []io.Reader{iotest.ErrReader(failure), io.MultiReader(strings.NewReader(`{} `), iotest.ErrReader(failure))} {
		err := Decode(reader, new(any))
		if !errors.Is(err, failure) || strings.Contains(err.Error(), failure.Error()) {
			t.Fatalf("reader failure: %v", err)
		}
	}
	if err := Decode(strings.NewReader(`{"enabled":`), new(any)); err == nil || err.Error() != "invalid JSON" {
		t.Fatalf("malformed JSON: %v", err)
	}
}
