package main

import (
	"encoding/binary"
	"math"
)

// --- Shared Configuration ---
const (
	// Hardware Optimization: Ryzen 9 7900X (12 Cores / 24 Threads)
	// We saturate all logical cores for parallel workloads.
	CPUThreads = 24

	Symbol  = "BTCUSDT"
	BaseDir = "data"

	// Default Targets
	TargetYear  = 2024
	TargetMonth = 1

	// Binary Format Constants
	// Floating point scalar for integer compression (1.00 = 100000000)
	PxScale = 100_000_000.0
	QtScale = 100_000_000.0

	// Magic headers for file identification
	AggMagic   = "AGG3"
	IdxMagic   = "QIDX"
	IdxVersion = 1

	// Binary Layout Sizes (Bytes)
	HeaderSize  = 48
	RowSize     = 48
	FeatureSize = 24
)

// --- Binary Structures ---

// AggHeader matches the binary layout for our compressed data files.
// Used by data.go for writing and build.go for reading.
type AggHeader struct {
	Magic    [4]byte
	Version  uint8
	Day      uint8
	ZLevel   uint16
	RowCount uint64
	MinTs    int64
	MaxTs    int64
	Padding  [16]byte
}

// --- Math Helpers ---
// Highly optimized, inlined-capable math functions used across the monolith.

// Mean calculates the arithmetic average of a slice.
func Mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0.0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// StdDev calculates sample standard deviation.
// Requires pre-calculated mean to avoid redundant iteration in tight loops.
func StdDev(vals []float64, mean float64) float64 {
	if len(vals) < 2 {
		return 0.0
	}
	sumSq := 0.0
	for _, v := range vals {
		d := v - mean
		sumSq += d * d
	}
	return math.Sqrt(sumSq / float64(len(vals)-1))
}

// Correlation calculates Pearson correlation coefficient (r).
func Correlation(x, y []float64) float64 {
	n := len(x)
	if n != len(y) || n == 0 {
		return 0.0
	}
	mx, my := Mean(x), Mean(y)
	sxx, syy, sxy := 0.0, 0.0, 0.0
	for i := 0; i < n; i++ {
		dx := x[i] - mx
		dy := y[i] - my
		sxx += dx * dx
		syy += dy * dy
		sxy += dx * dy
	}
	if sxx == 0 || syy == 0 {
		return 0.0
	}
	return sxy / math.Sqrt(sxx*syy)
}

// --- Binary Helpers ---

// PutRow efficiently packs standard AggTrade data into a byte slice.
// Uses LittleEndian to match x86_64 architecture (Ryzen 7900X) natively.
func PutRow(buf []byte, tid, px, qty, fid uint64, cnt uint32, flags uint16, ts int64) {
	binary.LittleEndian.PutUint64(buf[0:], tid)
	binary.LittleEndian.PutUint64(buf[8:], px)
	binary.LittleEndian.PutUint64(buf[16:], qty)
	binary.LittleEndian.PutUint64(buf[24:], fid)
	binary.LittleEndian.PutUint32(buf[32:], cnt)
	binary.LittleEndian.PutUint16(buf[36:], flags)
	binary.LittleEndian.PutUint64(buf[38:], uint64(ts))
}
