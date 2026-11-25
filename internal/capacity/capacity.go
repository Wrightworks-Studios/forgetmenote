// Package capacity owns operator resource budgets. These are deployment controls,
// not product cardinality limits or substitutes for a model's context window.
package capacity

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
)

type Budget string

const (
	FileBytes           Budget = "FILE_BYTES"
	SourceBytes         Budget = "SOURCE_BYTES"
	SourceTextBytes     Budget = "SOURCE_TEXT_BYTES"
	SourceJobBytes      Budget = "SOURCE_JOB_BYTES"
	SourceJobCount      Budget = "SOURCE_JOB_COUNT"
	BundleBytes         Budget = "BUNDLE_BYTES"
	ImportBytes         Budget = "IMPORT_BYTES"
	ImportObjects       Budget = "IMPORT_OBJECTS"
	IndexBytes          Budget = "INDEX_BYTES"
	HistoryBytes        Budget = "HISTORY_BYTES"
	StateBytes          Budget = "STATE_BYTES"
	ArtifactBytes       Budget = "ARTIFACT_BYTES"
	AutomationRuns      Budget = "AUTOMATION_RUNS"
	AutomationWorkers   Budget = "AUTOMATION_WORKERS"
	AutomationAIWorkers Budget = "AUTOMATION_AI_WORKERS"
	ReadRequests        Budget = "READ_REQUESTS"
	WriteRequests       Budget = "WRITE_REQUESTS"
	SourceRequests      Budget = "SOURCE_REQUESTS"
	DecodeRequests      Budget = "DECODE_REQUESTS"
	IdleEngines         Budget = "IDLE_ENGINES"
	IdleIndexBytes      Budget = "IDLE_INDEX_BYTES"
)

var ErrBusy = errors.New("request capacity reached; retry shortly")

var defaults = map[Budget]int{
	FileBytes: 32 << 20, SourceBytes: 64 << 20, SourceTextBytes: 4 << 20,
	SourceJobBytes: 2 << 30, SourceJobCount: 100,
	BundleBytes: 32 << 20, ImportBytes: 256 << 20, ImportObjects: 100000,
	IndexBytes: 512 << 20, HistoryBytes: 64 << 20, StateBytes: 64 << 20,
	ArtifactBytes: 32 << 20, AutomationRuns: 1000,
	AutomationWorkers: 4, AutomationAIWorkers: 8,
	ReadRequests: 0, WriteRequests: 0, SourceRequests: 0, DecodeRequests: 0,
	IdleEngines: 4, IdleIndexBytes: 64 << 20,
}

func (b Budget) Env() string { return "FMN_LIMIT_" + string(b) }

// Validate runs before opening state. Leave arithmetic headroom for encodings,
// lookahead reads and transient representations; there is no machine-independent
// upper ceiling. Limits must remain finite to protect a shared server.
func Validate() error {
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "FMN_LIMIT_") {
			continue
		}
		budget := Budget(strings.TrimPrefix(key, "FMN_LIMIT_"))
		if _, ok := defaults[budget]; !ok {
			return fmt.Errorf("unknown capacity setting %s", key)
		}
		if _, err := parse(value); err != nil {
			return fmt.Errorf("%s requires a positive integer with encoding headroom", key)
		}
	}
	return nil
}

func parse(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > math.MaxInt/64 {
		return 0, fmt.Errorf("invalid resource budget")
	}
	return n, nil
}

func (b Budget) Limit() int {
	if value, ok := os.LookupEnv(b.Env()); ok {
		if n, err := parse(value); err == nil {
			return n
		}
	}
	// Use the process's effective CPU allocation, including container-aware
	// GOMAXPROCS, while keeping default parallel I/O bounded on large hosts.
	cpus := runtime.GOMAXPROCS(0)
	switch b {
	case ReadRequests:
		return max(2, min(cpus*2, 32))
	case WriteRequests:
		return max(2, min(cpus, 16))
	case SourceRequests:
		return max(1, min(cpus/2, 4))
	case DecodeRequests:
		return max(2, min(cpus, 8))
	}
	return defaults[b]
}

func (b Budget) Error() error {
	return fmt.Errorf("configured resource budget exceeded (%s=%d); raise the operator setting or reduce this operation", b.Env(), b.Limit())
}
