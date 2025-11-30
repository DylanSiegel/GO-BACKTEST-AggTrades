package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

// --- Shared Configuration ---
const (
	CPUThreads = 24
	Symbol     = "BTCUSDT"
	BaseDir    = "data"

	// Default Target for Build/Study
	TargetYear  = 2024
	TargetMonth = 1

	// Constants for binary formats
	PxScale     = 100_000_000.0
	QtScale     = 100_000_000.0
	AggMagic    = "AGG3"
	IdxMagic    = "QIDX"
	IdxVersion  = 1
	HeaderSize  = 48
	RowSize     = 48
	FeatureSize = 24
)

// --- Entry Point (Dispatcher) ---

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run . [command]")
		fmt.Println("Commands:")
		fmt.Println("  data    - Download and process raw data")
		fmt.Println("  build   - Build features (BuildAlpha)")
		fmt.Println("  sanity  - Validate data integrity")
		fmt.Println("  study   - Analyze features (StudyAlpha)")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "data":
		runData()
	case "build":
		runBuild()
	case "sanity":
		runSanity()
	case "study":
		runStudy()
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

// --- Binary Structures ---

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

func PutRow(buf []byte, tid, px, qty, fid uint64, cnt uint32, flags uint16, ts int64) {
	binary.LittleEndian.PutUint64(buf[0:], tid)
	binary.LittleEndian.PutUint64(buf[8:], px)
	binary.LittleEndian.PutUint64(buf[16:], qty)
	binary.LittleEndian.PutUint64(buf[24:], fid)
	binary.LittleEndian.PutUint32(buf[32:], cnt)
	binary.LittleEndian.PutUint16(buf[36:], flags)
	binary.LittleEndian.PutUint64(buf[38:], uint64(ts))
}
