package benchfixture

import (
	"encoding/binary"
	"encoding/hex"
	"math/bits"
)

// rng is xoshiro256** seeded through splitmix64. math/rand does not promise
// stable output across releases, and floating point may fuse differently per
// architecture, so fixtures draw only integers from this generator.
type rng struct{ s [4]uint64 }

// newRNG derives an independent stream for one purpose, so adding draws to one
// part of a generator never shifts the output of another.
func newRNG(seed uint64, stream string) *rng {
	h := uint64(14695981039346656037)
	for i := 0; i < len(stream); i++ {
		h ^= uint64(stream[i])
		h *= 1099511628211
	}
	x := seed ^ h
	r := &rng{}
	for i := range r.s {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
		z = (z ^ z>>27) * 0x94d049bb133111eb
		r.s[i] = z ^ z>>31
	}
	return r
}

func (r *rng) next() uint64 {
	s := &r.s
	result := bits.RotateLeft64(s[1]*5, 7) * 9
	t := s[1] << 17
	s[2] ^= s[0]
	s[3] ^= s[1]
	s[1] ^= s[2]
	s[0] ^= s[3]
	s[2] ^= t
	s[3] = bits.RotateLeft64(s[3], 45)
	return result
}

// intn returns a uniform integer in [0, n) without modulo bias (Lemire).
func (r *rng) intn(n int) int {
	if n <= 0 {
		panic("benchfixture: intn of non-positive bound")
	}
	bound := uint64(n)
	hi, lo := bits.Mul64(r.next(), bound)
	if lo < bound {
		threshold := -bound % bound
		for lo < threshold {
			hi, lo = bits.Mul64(r.next(), bound)
		}
	}
	return int(hi)
}

// between returns a uniform integer in [lo, hi].
func (r *rng) between(lo, hi int) int { return lo + r.intn(hi-lo+1) }

// permille reports true with probability p/1000.
func (r *rng) permille(p int) bool { return r.intn(1000) < p }

// weighted picks an index with probability proportional to its weight.
func (r *rng) weighted(weights []int) int {
	total := 0
	for _, w := range weights {
		total += w
	}
	n := r.intn(total)
	for i, w := range weights {
		if n < w {
			return i
		}
		n -= w
	}
	return len(weights) - 1
}

func (r *rng) shuffle(n int, swap func(i, j int)) {
	for i := n - 1; i > 0; i-- {
		swap(i, r.intn(i+1))
	}
}

func (r *rng) fill(b []byte) {
	i := 0
	for ; i+8 <= len(b); i += 8 {
		binary.LittleEndian.PutUint64(b[i:], r.next())
	}
	if i < len(b) {
		var tail [8]byte
		binary.LittleEndian.PutUint64(tail[:], r.next())
		copy(b[i:], tail[:])
	}
}

// hexID returns n random bytes as lowercase hexadecimal, the shape of note,
// account, comment and Trash IDs.
func (r *rng) hexID(n int) string {
	b := make([]byte, n)
	r.fill(b)
	return hex.EncodeToString(b)
}

func (r *rng) permutation(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	r.shuffle(n, func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func pick[T any](r *rng, items []T) T { return items[r.intn(len(items))] }
