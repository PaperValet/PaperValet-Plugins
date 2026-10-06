package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	mrand "math/rand"
)

// randIntn returns a cryptographically random int in [0, n). It falls back
// to a time-seeded math/rand only if crypto/rand fails, which should not
// happen on Linux.
func randIntn(n int) int {
	if n <= 1 {
		return 0
	}
	// Rejection sampling keeps the distribution uniform. The domain is the
	// full uint64 range with the incomplete tail block dropped, so on
	// average one draw in 2^64/n is rejected (negligible).
	limit := math.MaxUint64 - math.MaxUint64%uint64(n)
	for {
		v, err := cryptoUint64()
		if err != nil {
			seedRand()
			return mrand.Intn(n)
		}
		if v < limit {
			return int(v % uint64(n))
		}
	}
}

func cryptoUint64() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("crypto/rand unavailable: %w", err)
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

var seeded = mrand.New(mrand.NewSource(1))

func seedRand() {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		seeded = mrand.New(mrand.NewSource(int64(binary.LittleEndian.Uint64(b[:]))))
	}
}

// randID returns a random message id for Telegram requests.
func randID() int64 {
	if v, err := cryptoUint64(); err == nil {
		return int64(v)
	}
	return seeded.Int63()
}
