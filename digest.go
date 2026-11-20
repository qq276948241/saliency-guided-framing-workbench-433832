/*
 * Copyright (c) 2014-2026 Christian Muehlhaeuser
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 */

package smartcrop

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"image"
	"math"
)

// digestPixels hashes the canonical RGBA buffer in a fixed order. The digest is
// stable across runs and platforms because it only depends on the byte content.
func digestPixels(img *image.RGBA) string {
	h := sha256.New()
	if img == nil {
		return hex.EncodeToString(h.Sum(nil))
	}
	b := img.Bounds()
	var buf [8]byte
	binary.LittleEndian.PutUint32(buf[0:4], uint32(b.Dx()))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(b.Dy()))
	_, _ = h.Write(buf[:])

	row := make([]byte, b.Dx()*4)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		start := img.PixOffset(b.Min.X, y)
		copy(row, img.Pix[start:start+b.Dx()*4])
		_, _ = h.Write(row)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashStrings folds an ordered list of strings into a sha256 digest.
func hashStrings(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(len(p)))
		_, _ = h.Write(buf[:])
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// splitmix64 is a tiny deterministic PRNG. It has no global state and is safe
// to use from independent generators, so candidate generation only depends on
// the configured seed.
type splitmix64 struct {
	state uint64
}

func newSplitmix64(seed uint64) *splitmix64 {
	return &splitmix64{state: seed}
}

func (s *splitmix64) next() uint64 {
	s.state += 0x9E3779B97F4A7C15
	z := s.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// nextInt returns a value in [0, n). It panics if n <= 0, which the candidate
// generator guards against.
func (s *splitmix64) nextInt(n int) int {
	if n <= 0 {
		return 0
	}
	return int(s.next() % uint64(n))
}

// roundHalfUp maps a float dimension to an integer, rounding halves up. It is
// the single rounding rule used for every candidate size in the scoring
// module; it is intentionally not math.Round (which rounds halves away from
// zero) so negative values can never appear.
func roundHalfUp(v float64) int {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return int(math.Floor(v + 0.5))
}

// clampInt bounds v to [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
