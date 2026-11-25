//go:build !fmn_omit_outlook

package sources

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
	"strings"
	"testing"
)

func TestNativeOutlookMessage(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	raw, err := os.ReadFile("testdata/reference.msg")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Prepare(t.Context(), "message.msg", "", raw)
	if err != nil || !strings.Contains(got.Text, "Amber meeting") || !strings.Contains(got.Text, "Cobalt evidence") || string(got.Original) != string(raw) {
		t.Fatalf("native Outlook import lost headers/body/original: %q %v", got.Text, err)
	}
}

func TestNativeOutlookRejectsDeepDirectoriesBeforeTraversal(t *testing.T) {
	// A valid, small CFB directory chain. The old constructor accepts it and
	// retains a copied ancestor path for every storage entry.
	const entries = 100
	const sectors = (entries + 3) / 4
	reference, err := os.ReadFile("testdata/reference.msg")
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, (2+sectors)*512)
	copy(raw, reference[:512])
	binary.LittleEndian.PutUint32(raw[48:52], 1)
	binary.LittleEndian.PutUint32(raw[60:64], 0xfffffffe)
	binary.LittleEndian.PutUint32(raw[64:68], 0)
	for i := 0; i < 128; i++ {
		binary.LittleEndian.PutUint32(raw[512+i*4:], 0xffffffff)
	}
	binary.LittleEndian.PutUint32(raw[512:], 0xfffffffd)
	for i := 1; i <= sectors; i++ {
		next := uint32(i + 1)
		if i == sectors {
			next = 0xfffffffe
		}
		binary.LittleEndian.PutUint32(raw[512+i*4:], next)
	}
	for i := 0; i < entries; i++ {
		entry := raw[1024+i*128 : 1024+(i+1)*128]
		name, _ := utf16le("Storage\x00")
		copy(entry, name)
		binary.LittleEndian.PutUint16(entry[64:], uint16(len(name)))
		entry[66], entry[67] = 1, 1
		if i == 0 {
			entry[66] = 5
		}
		for _, offset := range []int{68, 72, 76} {
			binary.LittleEndian.PutUint32(entry[offset:], 0xffffffff)
		}
		if i+1 < entries {
			binary.LittleEndian.PutUint32(entry[76:], uint32(i+1))
		}
		binary.LittleEndian.PutUint32(entry[116:], 0xfffffffe)
	}
	if _, err := Prepare(t.Context(), "nested.msg", "", raw); err == nil {
		t.Fatal("deep directory accepted before bounded traversal")
	}
}

func TestNativeOutlookBodyFallbackAndCorruptCounts(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	raw, err := os.ReadFile("testdata/reference.msg")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"html", "rtf"} {
		t.Run(kind, func(t *testing.T) {
			data := append([]byte(nil), raw...)
			property := "10130102"
			body := []byte(`<p>Cobalt evidence</p><script>unsafe()</script>`)
			if kind == "rtf" {
				property = "10090102"
				rtf := []byte(`{\rtf1\ansi {\fonttbl hidden}Cobalt\tab evidence\par \u945?}`)
				body = make([]byte, 16+len(rtf))
				binary.LittleEndian.PutUint32(body[:4], uint32(len(body)-4))
				binary.LittleEndian.PutUint32(body[4:8], uint32(len(rtf)))
				copy(body[8:12], "MELA")
				copy(body[16:], rtf)
			}
			before, _ := utf16le("__substg1.0_1000001F")
			after, _ := utf16le("__substg1.0_" + property)
			data = bytes.ReplaceAll(data, before, after)
			// Fixture directory entry 3 points to mini-stream offset 128.
			binary.LittleEndian.PutUint64(data[1528:1536], uint64(len(body)))
			clear(data[2176:2304])
			copy(data[2176:], body)
			got, err := Prepare(t.Context(), "message.msg", "", data)
			if err != nil || !strings.Contains(got.Text, "Cobalt") || !strings.Contains(got.Text, "evidence") || strings.Contains(got.Text, "unsafe") || strings.Contains(got.Text, "hidden") {
				t.Fatalf("%s body fallback: %q %v", kind, got.Text, err)
			}
			if kind == "rtf" && !strings.Contains(got.Text, "α") {
				t.Fatalf("Unicode RTF body lost: %q", got.Text)
			}
		})
	}
	bad := append([]byte(nil), raw...)
	binary.LittleEndian.PutUint32(bad[64:68], 0xffffffff)
	if _, err := Prepare(t.Context(), "bad.msg", "", bad); err == nil {
		t.Fatal("unbounded compound allocation accepted")
	}
}

func TestNativeOutlookCompressedDictionaryBody(t *testing.T) {
	raw, err := os.ReadFile("testdata/reference.msg")
	if err != nil {
		t.Fatal(err)
	}
	// One dictionary reference expands MS-OXRTFCP's standard initial RTF header;
	// the rest is literal data followed by the protocol's end-of-dictionary token.
	text := []byte(`{\rtf1\ansi Cobalt compressed evidence}`)
	prefix := 10
	type token struct {
		reference bool
		data      []byte
	}
	tokens := []token{{true, []byte{0, byte(prefix - 2)}}}
	for _, b := range text[prefix:] {
		tokens = append(tokens, token{false, []byte{b}})
	}
	end := uint16(207+len(text)) << 4
	tokens = append(tokens, token{true, []byte{byte(end >> 8), byte(end)}})
	var payload []byte
	for start := 0; start < len(tokens); start += 8 {
		group := tokens[start:min(start+8, len(tokens))]
		var flags byte
		for i, item := range group {
			if item.reference {
				flags |= 1 << i
			}
		}
		payload = append(payload, flags)
		for _, item := range group {
			payload = append(payload, item.data...)
		}
	}
	body := make([]byte, 16+len(payload))
	binary.LittleEndian.PutUint32(body[:4], uint32(len(body)-4))
	binary.LittleEndian.PutUint32(body[4:8], uint32(len(text)))
	copy(body[8:12], "LZFu")
	copy(body[16:], payload)
	// This protocol uses CRC initialization zero, without the usual final XOR.
	checksum := crc32.Update(0xffffffff, crc32.MakeTable(crc32.IEEE), payload) ^ 0xffffffff
	binary.LittleEndian.PutUint32(body[12:16], checksum)
	before, _ := utf16le("__substg1.0_1000001F")
	after, _ := utf16le("__substg1.0_10090102")
	raw = bytes.ReplaceAll(raw, before, after)
	binary.LittleEndian.PutUint64(raw[1528:1536], uint64(len(body)))
	clear(raw[2176:2304])
	copy(raw[2176:], body)
	got, err := Prepare(t.Context(), "compressed.msg", "", raw)
	if err != nil || !strings.Contains(got.Text, "Cobalt compressed evidence") {
		t.Fatalf("compressed MSG dictionary body: %q %v", got.Text, err)
	}
	raw[2176+12] ^= 1
	if _, err := Prepare(t.Context(), "corrupt.msg", "", raw); err == nil {
		t.Fatal("corrupt compressed MSG CRC accepted")
	}
}
