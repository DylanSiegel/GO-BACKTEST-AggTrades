package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// --- Configuration ---
// We study 3 distinct horizons to check Alpha Decay
var Horizons = []int{20, 50, 100}

// REALITY CHECK: Execution Latency (Network + Gateway + Matching Engine)
const ExecutionLagMS = 70 // 70ms accurate lag

func runStudy() {
	start := time.Now()
	featDir := filepath.Join(BaseDir, "features", Symbol)

	// 1. Gather Files
	var files []string
	years, _ := os.ReadDir(featDir)
	for _, y := range years {
		if y.IsDir() {
			months, _ := os.ReadDir(filepath.Join(featDir, y.Name()))
			for _, m := range months {
				if m.IsDir() {
					days, _ := os.ReadDir(filepath.Join(featDir, y.Name(), m.Name()))
					for _, d := range days {
						if filepath.Ext(d.Name()) == ".bin" {
							files = append(files, filepath.Join(featDir, y.Name(), m.Name(), d.Name()))
						}
					}
				}
			}
		}
	}

	fmt.Printf("--- study.go | Analyzing %d Days | Lag: %dms ---\n", len(files), ExecutionLagMS)

	// 2. Parallel Processing (Ryzen 7900X Saturation)
	jobs := make(chan string, len(files))
	// FIX: Use AlphaResult (from scorecard.go), not reportEntry
	results := make(chan AlphaResult, len(files))
	var wg sync.WaitGroup

	// Workers
	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				results <- analyzeFile(path)
			}
		}()
	}

	for _, f := range files {
		jobs <- f
	}
	close(jobs)
	wg.Wait()
	close(results)

	// 3. Aggregation
	var report []AlphaResult
	meanIC50 := 0.0
	meanBE50 := 0.0
	valid := 0

	for r := range results {
		if r.NBars > 0 {
			report = append(report, r)
			if h50, ok := r.Horizons["50"]; ok {
				meanIC50 += h50.ICPearson
				meanBE50 += h50.BreakevenBps
				valid++
			}
		}
	}

	// Sort by date for the JSON report
	sort.Slice(report, func(i, j int) bool { return report[i].Date < report[j].Date })

	if valid > 0 {
		fmt.Printf("DONE in %s\n", time.Since(start))
		fmt.Printf("Global Mean IC (50 ticks, %dms lag): %.4f\n", ExecutionLagMS, meanIC50/float64(valid))
		fmt.Printf("Global Mean Breakeven (50 ticks): %.4f bps\n", meanBE50/float64(valid))
	}

	outPath := filepath.Join(BaseDir, "reports", fmt.Sprintf("alpha_core_%s.json", Symbol))
	os.MkdirAll(filepath.Dir(outPath), 0755)

	// FIX: SaveReport now accepts []AlphaResult correctly
	SaveReport(report, outPath)
	fmt.Println("Report saved:", outPath)
}

// FIX: Return AlphaResult to match scorecard.go
func analyzeFile(path string) AlphaResult {
	// 1. Read Binary
	data, err := os.ReadFile(path)
	if err != nil {
		return AlphaResult{NBars: 0}
	}

	n := len(data) / FeatureSize
	maxHorizon := Horizons[len(Horizons)-1]

	// Basic length check (very rough)
	if n < maxHorizon+100 {
		return AlphaResult{NBars: 0}
	}

	// 2. Unpack into Separate Slices (SoA layout for cache efficiency)
	// We need Timestamps now for accurate lagging
	timestamps := make([]int64, n)
	prices := make([]float64, n)
	signals := make([]float64, n)

	for i := 0; i < n; i++ {
		off := i * FeatureSize
		// Layout: [Ts(8) | Px(8) | Sig(8)]
		tsRaw := binary.LittleEndian.Uint64(data[off:])
		pxRaw := binary.LittleEndian.Uint64(data[off+8:])
		sigRaw := binary.LittleEndian.Uint64(data[off+16:])

		timestamps[i] = int64(tsRaw)
		prices[i] = math.Float64frombits(pxRaw)
		signals[i] = math.Float64frombits(sigRaw)
	}

	// 3. Date ID
	base := filepath.Base(path)
	dir := filepath.Base(filepath.Dir(path))
	year := filepath.Base(filepath.Dir(filepath.Dir(path)))
	dateID := fmt.Sprintf("%s-%s-%s", year, dir, base[:len(base)-4])

	// FIX: Use struct from scorecard.go
	res := AlphaResult{
		Date:          dateID,
		NBars:         n,
		SignalQuality: AnalyzeSignalQuality(signals), // This now matches types
		Horizons:      make(map[string]Horizon),      // This now matches types
	}

	// 4. Horizon Loops with TIME-BASED LAG
	for _, h := range Horizons {
		var subSig []float64
		var futureRet []float64

		// Pre-allocate to avoid resize overhead (guess approx size)
		estSize := n - h
		if estSize > 0 {
			subSig = make([]float64, 0, estSize)
			futureRet = make([]float64, 0, estSize)
		}

		// Sliding Window Cursor for Entry
		entryIdx := 0

		for i := 0; i < n; i++ {
			// Signal is generated at Time(i)
			genTime := timestamps[i]

			// We cannot execute until Time(i) + 70ms
			targetEntryTime := genTime + ExecutionLagMS

			// Fast-forward the entry cursor to find the first trade >= targetEntryTime
			if entryIdx < i {
				entryIdx = i
			}
			for entryIdx < n && timestamps[entryIdx] < targetEntryTime {
				entryIdx++
			}

			// If entry index + horizon is out of bounds, we stop processing this horizon
			if entryIdx+h >= n {
				break
			}

			// Capture valid pair
			// Signal was from index 'i'
			// Entry Price is at 'entryIdx' (Delayed by 70ms)
			// Exit Price is at 'entryIdx + h' (N ticks after Entry)

			pEntry := prices[entryIdx]
			pExit := prices[entryIdx+h]

			if pEntry > 0 {
				subSig = append(subSig, signals[i])
				futureRet = append(futureRet, (pExit-pEntry)/pEntry)
			}
		}

		// Calc stats for this horizon
		// FIX: Correct types passed to CalculateAlphaMetrics
		res.Horizons[fmt.Sprintf("%d", h)] = CalculateAlphaMetrics(subSig, futureRet)
	}

	return res
}
