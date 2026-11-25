package workspace

import (
	"errors"
	"unicode/utf8"
)

// Yjs/lib0 uses JavaScript numbers for identifiers and clocks. Distinct larger
// integers can decode to the same value even though Go represents them exactly.
const maxYjsInteger = uint64(1<<53 - 1)

// Only the Yjs V1 subset for a single plain Y.Text is accepted. Rich formatting
// remains Markdown. This prevents invisible maps, embeds, nested types, and
// unrelated root documents from entering the durable collaboration record.
func validateMarkdownUpdate(raw []byte) error {
	r := yReader{raw: raw}
	clients := r.uint()
	for c := uint64(0); c < clients && r.err == nil; c++ {
		count := r.uint()
		r.uint()
		clock := r.uint() // struct count, client, starting clock
		if count > 100000 {
			return errors.New("too many Yjs structures")
		}
		for n := uint64(0); n < count && r.err == nil; n++ {
			info := r.byte()
			if info == 0 || info == 10 {
				clock = r.rangeEnd(clock, r.uint())
				continue
			}
			if info&32 != 0 {
				return errors.New("Yjs maps are not Markdown")
			}
			if info&128 != 0 {
				r.uint()
				r.rangeEnd(r.uint(), 1)
			}
			if info&64 != 0 {
				r.uint()
				r.rangeEnd(r.uint(), 1)
			}
			if info&192 == 0 {
				if r.uint() != 1 || r.string() != "markdown" {
					return errors.New("Yjs update must target only markdown text")
				}
			}
			switch info & 31 {
			case 1:
				clock = r.rangeEnd(clock, r.uint())
			case 4:
				length := uint64(0)
				for _, ch := range r.string() {
					length++
					if ch > 0xffff {
						length++ // Y.Text clocks count UTF-16 code units.
					}
				}
				clock = r.rangeEnd(clock, length)
			default:
				return errors.New("Yjs update must contain plain Markdown text")
			}
		}
	}
	deletes := r.uint()
	for c := uint64(0); c < deletes && r.err == nil; c++ {
		r.uint()
		count := r.uint()
		if count > 100000 {
			return errors.New("too many Yjs deletions")
		}
		for n := uint64(0); n < count && r.err == nil; n++ {
			clock, length := r.uint(), r.uint()
			r.rangeEnd(clock, length)
		}
	}
	if r.err != nil || r.at != len(raw) {
		return errors.New("invalid Yjs V1 update")
	}
	return nil
}

type yReader struct {
	raw []byte
	at  int
	err error
}

func (r *yReader) byte() byte {
	if r.at >= len(r.raw) {
		r.err = errors.New("truncated update")
		return 0
	}
	b := r.raw[r.at]
	r.at++
	return b
}
func (r *yReader) uint() uint64 {
	var v uint64
	for n := uint(0); n < 56 && r.err == nil; n += 7 {
		b := r.byte()
		v |= uint64(b&127) << n
		if v > maxYjsInteger {
			r.err = errors.New("Yjs integer exceeds JavaScript's safe range")
			return 0
		}
		if b < 128 {
			return v
		}
	}
	r.err = errors.New("invalid integer")
	return 0
}
func (r *yReader) rangeEnd(clock, length uint64) uint64 {
	if r.err != nil {
		return 0
	}
	// Bound the exclusive end too: it becomes the next struct's clock or a
	// state-vector entry. Subtraction prevents overflow before validation.
	if clock > maxYjsInteger || length > maxYjsInteger-clock {
		r.err = errors.New("Yjs clock range exceeds JavaScript's safe range")
		return 0
	}
	return clock + length
}
func (r *yReader) string() string {
	n := r.uint()
	if n > uint64(len(r.raw)-r.at) {
		r.err = errors.New("invalid text length")
		return ""
	}
	text := string(r.raw[r.at : r.at+int(n)])
	r.at += int(n)
	if !utf8.ValidString(text) {
		r.err = errors.New("invalid UTF-8")
	}
	return text
}
