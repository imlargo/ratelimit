package ratelimit

// SipHash-1-3, keyed, 64 bit output.
//
// The fingerprint of a key has two requirements that pull against each other,
// and hash/maphash can only satisfy one of them:
//
//   - An attacker must not be able to find two keys with the same fingerprint,
//     because sharing a cell with a victim means draining the victim's quota.
//     That rules out an unkeyed hash such as FNV.
//   - In distributed mode every node must compute the *same* fingerprint for
//     the same key, or the backend cannot correlate them at all. That rules out
//     hash/maphash, whose seed is per process and cannot be set.
//
// A keyed hash satisfies both: the key is random per process by default, and
// derived from a shared cluster key when a backend is configured. SipHash is
// the standard construction for exactly this - it is what Rust and Python use
// to make their hash tables collision-attack resistant - and the 1-3 variant is
// the fast one, intended for short inputs, which is all a rate limit key ever
// is.
//
// The implementation reads its input a byte at a time rather than reinterpreting
// the string's memory as a slice. It costs a few nanoseconds and it keeps
// unsafe out of a package whose job is being a security control.

const (
	sipC0 = 0x736f6d6570736575
	sipC1 = 0x646f72616e646f6d
	sipC2 = 0x6c7967656e657261
	sipC3 = 0x7465646279746573
)

// hashKey is a 128 bit SipHash key.
type hashKey struct{ k0, k1 uint64 }

func rotl(x uint64, b uint) uint64 { return x<<b | x>>(64-b) }

// sipString computes SipHash-1-3 of s under key k.
func sipString(k hashKey, s string) uint64 {
	v0 := k.k0 ^ sipC0
	v1 := k.k1 ^ sipC1
	v2 := k.k0 ^ sipC2
	v3 := k.k1 ^ sipC3

	n := len(s)
	i := 0
	for ; i+8 <= n; i += 8 {
		m := le64str(s, i)
		v3 ^= m
		// c = 1 compression round
		v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
		v0 ^= m
	}

	// The trailing partial block, with the length in the top byte.
	var b uint64 = uint64(n) << 56
	for j := 0; i+j < n; j++ {
		b |= uint64(s[i+j]) << (8 * uint(j))
	}
	v3 ^= b
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	v0 ^= b

	v2 ^= 0xff
	// d = 3 finalisation rounds
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)

	return v0 ^ v1 ^ v2 ^ v3
}

// sipBytes is sipString for a byte slice, used for addresses.
func sipBytes(k hashKey, p []byte) uint64 {
	v0 := k.k0 ^ sipC0
	v1 := k.k1 ^ sipC1
	v2 := k.k0 ^ sipC2
	v3 := k.k1 ^ sipC3

	n := len(p)
	i := 0
	for ; i+8 <= n; i += 8 {
		m := le64(p, i)
		v3 ^= m
		v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
		v0 ^= m
	}
	var b uint64 = uint64(n) << 56
	for j := 0; i+j < n; j++ {
		b |= uint64(p[i+j]) << (8 * uint(j))
	}
	v3 ^= b
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	v0 ^= b

	v2 ^= 0xff
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)

	return v0 ^ v1 ^ v2 ^ v3
}

func sipRound(v0, v1, v2, v3 uint64) (uint64, uint64, uint64, uint64) {
	v0 += v1
	v1 = rotl(v1, 13)
	v1 ^= v0
	v0 = rotl(v0, 32)

	v2 += v3
	v3 = rotl(v3, 16)
	v3 ^= v2

	v0 += v3
	v3 = rotl(v3, 21)
	v3 ^= v0

	v2 += v1
	v1 = rotl(v1, 17)
	v1 ^= v2
	v2 = rotl(v2, 32)

	return v0, v1, v2, v3
}

func le64str(s string, i int) uint64 {
	return uint64(s[i]) | uint64(s[i+1])<<8 | uint64(s[i+2])<<16 | uint64(s[i+3])<<24 |
		uint64(s[i+4])<<32 | uint64(s[i+5])<<40 | uint64(s[i+6])<<48 | uint64(s[i+7])<<56
}

func le64(p []byte, i int) uint64 {
	return uint64(p[i]) | uint64(p[i+1])<<8 | uint64(p[i+2])<<16 | uint64(p[i+3])<<24 |
		uint64(p[i+4])<<32 | uint64(p[i+5])<<40 | uint64(p[i+6])<<48 | uint64(p[i+7])<<56
}
