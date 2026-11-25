package knowledge

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInvalidBinaryCountAllocation(t *testing.T) {
	t.Setenv("FMN_LIMIT_INDEX_BYTES", "16777216")
	engine, _ := indexFixture(t)
	cfg := testConfig()
	cfg.Dimensions = 1
	if _, err := engine.Configure(t.Context(), cfg, false); err != nil {
		t.Fatal(err)
	}
	engine.Close()
	count := 1 << 20
	header, err := json.Marshal(vectorDiskHeader{Version: 2, ConfigID: cfg.ID(), Dimensions: 1, Count: count})
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(engine.dir, "cache")
	if err = os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(cache, "semantic.bin"))
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString(vectorMagic)
	binary.Write(file, binary.LittleEndian, uint32(len(header)))
	file.Write(header)
	// Sparse zero rows are invalid at the very first key. The file length only
	// satisfies the reader's theoretical minimum-record calculation.
	size := int64(len(vectorMagic) + 4 + len(header) + count*6 + 32)
	if err = file.Truncate(size); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	// import-files reads former vector files with this reader.
	_, _, _, err = readStoredVectors(filepath.Join(cache, "semantic.bin"))
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("invalid first record was accepted")
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("the vector file reader rejected an invalid first record after allocating %d bytes for a %d-byte sparse file under a16MiB file budget", allocated, size)
	if allocated > 32<<20 {
		t.Fatalf("invalid header count allocated %d bytes before rejecting the first record", allocated)
	}
}
