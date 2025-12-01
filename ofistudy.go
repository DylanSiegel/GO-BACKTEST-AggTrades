package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// --- Study Config ---

const (
	OOSDateStr   = "2024-01-01"
	StudyThreads = 24         // Saturate all 24 logical cores on Ryzen 9 7900X
	StudyMaxRows = 10_000_000 // Per-thread default row budget for buffers
)

// TimeHorizonsSec: The horizons to test.
var TimeHorizonsSec = []int{10, 30, 60, 180, 300}

var oosBoundaryYMD int

func init() {
	oosBoundaryYMD = parseOOSBoundary(OOSDateStr)
}

// --- Data Structures ---

// Accumulator aggregates stats across days for a single horizon.
type Accumulator struct {
	Days      int
	SumIC     float64
	SumSharpe float64
	SumHit    float64
	SumBE     float64
}

// DayResult holds one day's stats for ALL variants and ALL horizons.
// Map key: VariantID -> []MetricStats (index corresponds to TimeHorizonsSec)
type DayResult struct {
	Stats map[string][]MetricStats
	YMD   int
}

// --- Main Runner ---

func runStudy() {
	startT := time.Now()
	featRoot := filepath.Join(BaseDir, "features", Symbol)

	// 1. Discover Variants
	entries, err := os.ReadDir(featRoot)
	if err != nil {
		fmt.Printf("[err] reading feature dir: %v\n", err)
		return
	}
	var variants []string
	for _, e := range entries {
		if e.IsDir() {
			variants = append(variants, e.Name())
		}
	}
	slices.Sort(variants)

	if len(variants) == 0 {
		fmt.Println("[warn] No variants found.")
		return
	}

	fmt.Printf("--- OFI STUDY | %s | %d Variants | Split: %s ---\n", Symbol, len(variants), OOSDateStr)
	fmt.Printf("[arch] Optimized Day-Parallel Pipeline (Ryzen 7900X)\n")

	// 2. Discover Common Days (intersection of Raw Data and Features)
	tasks := discoverStudyDays(filepath.Join(featRoot, variants[0]))
	if len(tasks) == 0 {
		fmt.Println("[warn] No .bin days found for first variant.")
		return
	}

	workerCount := StudyThreads
	if workerCount > CPUThreads {
		workerCount = CPUThreads
	}
	fmt.Printf("[job] Processing %d days using %d threads.\n", len(tasks), workerCount)

	// 3. Worker Pool (Day-Parallel)
	resultsChan := make(chan DayResult, len(tasks))
	jobsChan := make(chan int, len(tasks))
	var wg sync.WaitGroup

	// Accumulators: Variant -> Horizon -> Accumulator
	isAcc := make(map[string][]Accumulator)
	oosAcc := make(map[string][]Accumulator)

	for _, v := range variants {
		isAcc[v] = make([]Accumulator, len(TimeHorizonsSec))
		oosAcc[v] = make([]Accumulator, len(TimeHorizonsSec))
	}

	// Spin up workers
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// --- Thread-Local Buffers (reused across days) ---
			maxRows := StudyMaxRows

			prices := make([]float64, maxRows)
			times := make([]int64, maxRows)
			sigBuf := make([]float64, maxRows)

			// Scratch buffers for per-horizon calculations
			scratchSig := make([]float64, 0, maxRows)
			scratchRet := make([]float64, 0, maxRows)

			// File read buffer (reused for signal files)
			fileBuf := make([]byte, maxRows*8)

			for idx := range jobsChan {
				dayInt := tasks[idx]
				res := processStudyDay(
					dayInt, variants, featRoot,
					prices, times, sigBuf, fileBuf,
					&scratchSig, &scratchRet,
				)
				resultsChan <- res
			}
		}()
	}

	// Enqueue jobs
	for i := range tasks {
		jobsChan <- i
	}
	close(jobsChan)

	// Close results channel when workers finish
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// 4. Aggregation (Main Thread)
	for res := range resultsChan {
		if len(res.Stats) == 0 {
			continue
		}
		isOOS := res.YMD >= oosBoundaryYMD

		for v, hStats := range res.Stats {
			for hIdx, stat := range hStats {
				if stat.Count == 0 {
					continue
				}

				var target []Accumulator
				if isOOS {
					target = oosAcc[v]
				} else {
					target = isAcc[v]
				}

				target[hIdx].Days++
				target[hIdx].SumIC += stat.ICPearson
				target[hIdx].SumSharpe += stat.SharpeAnnual
				target[hIdx].SumHit += stat.HitRate
				target[hIdx].SumBE += stat.BreakevenBps
			}
		}
	}

	// 5. Reporting
	fmt.Println()
	for hIdx, sec := range TimeHorizonsSec {
		printHorizonTable(sec, variants, isAcc, oosAcc, hIdx)
		fmt.Println()
	}

	fmt.Printf("[study] Complete in %s\n", time.Since(startT))
}

// processStudyDay loads Raw Data ONCE, then iterates variants.
func processStudyDay(
	dayInt int,
	variants []string,
	featRoot string,
	prices []float64,
	times []int64,
	sigBuf []float64,
	fileBuf []byte,
	scratchSig, scratchRet *[]float64,
) DayResult {
	y := dayInt / 10000
	m := (dayInt % 10000) / 100
	d := dayInt % 100

	res := DayResult{
		YMD:   dayInt,
		Stats: make(map[string][]MetricStats),
	}

	// 1. Load Raw Data (Expensive ZLIB op - Done ONCE)
	rawBytes, rowCount, ok := loadRawDay(y, m, d)
	if !ok || rowCount == 0 {
		return res
	}

	n := int(rowCount)

	// --- CRITICAL FIX: Grow buffers instead of dropping high-vol days ---
	if n > cap(prices) {
		newCap := n + n/4 // 25% headroom
		prices = make([]float64, newCap)
		times = make([]int64, newCap)
		sigBuf = make([]float64, newCap)
	}
	if n > cap(*scratchSig) {
		*scratchSig = make([]float64, 0, n+n/4)
	}
	if n > cap(*scratchRet) {
		*scratchRet = make([]float64, 0, n+n/4)
	}
	if n*8 > cap(fileBuf) {
		fileBuf = make([]byte, n*8+n*2)
	}

	// Reslice working views to exactly n rows
	prices = prices[:n]
	times = times[:n]
	sigBuf = sigBuf[:n]

	// 2. Parse Raw Data (Vectorized parsing)
	for i := 0; i < n; i++ {
		off := i * RowSize // RowSize=48 defined in common.go
		// Prices at offset 8 (uint64), Times at offset 38 (uint64)
		pBits := binary.LittleEndian.Uint64(rawBytes[off+8:])
		tBits := binary.LittleEndian.Uint64(rawBytes[off+38:])

		prices[i] = float64(pBits) // PxScale is constant; relative moves are what matter
		times[i] = int64(tBits)
	}

	// 3. Iterate Variants
	dStr := fmt.Sprintf("%04d%02d%02d", y, m, d)

	for _, v := range variants {
		sigPath := filepath.Join(featRoot, v, dStr+".bin")

		// Fast Load Signals
		loadedSigs, ok := fastLoadFloats(sigPath, fileBuf, sigBuf)
		if !ok || len(loadedSigs) != n {
			continue
		}

		// Calc Stats for all horizons
		statsList := make([]MetricStats, len(TimeHorizonsSec))
		for hIdx, sec := range TimeHorizonsSec {
			statsList[hIdx] = calcDailyStatsTimePrepared(
				loadedSigs, prices, times, sec*1000, scratchSig, scratchRet,
			)
		}
		res.Stats[v] = statsList
	}

	return res
}

// fastLoadFloats reads binary floats into a pre-allocated buffer to reduce GC.
func fastLoadFloats(path string, fileBuf []byte, outBuf []float64) ([]float64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	// Stat to get size
	fi, err := f.Stat()
	if err != nil {
		return nil, false
	}
	size := int(fi.Size())
	if size <= 0 || size%8 != 0 {
		return nil, false
	}
	count := size / 8

	if count > cap(outBuf) {
		// Should be extremely rare if StudyMaxRows is set appropriately.
		outBuf = make([]float64, count)
	} else {
		outBuf = outBuf[:count]
	}

	// Ensure fileBuf is large enough
	if cap(fileBuf) < size {
		fileBuf = make([]byte, size)
	}
	fileBuf = fileBuf[:size]

	// Single Syscall Read using io.ReadFull
	if _, err := io.ReadFull(f, fileBuf); err != nil {
		return nil, false
	}

	// Binary Decode
	for i := 0; i < count; i++ {
		bits := binary.LittleEndian.Uint64(fileBuf[i*8:])
		outBuf[i] = math.Float64frombits(bits)
	}

	return outBuf, true
}

// calcDailyStatsTimePrepared matches input logic but uses pointers for scratch buffers.
func calcDailyStatsTimePrepared(
	sig []float64,
	prices []float64,
	times []int64,
	horizonMs int,
	scratchSig *[]float64,
	scratchRet *[]float64,
) MetricStats {
	n := len(sig)
	if n < 200 || len(prices) != n || len(times) != n {
		return MetricStats{}
	}

	// Reset scratch length, keep capacity
	vSig := (*scratchSig)[:0]
	vRet := (*scratchRet)[:0]

	j := 0
	hVal := int64(horizonMs)

	for i := 0; i < n; i++ {
		pStart := prices[i]
		s := sig[i]

		// Fast reject: invalid price or zero signal
		if pStart <= 0 || s == 0 {
			continue
		}

		t0 := times[i]
		target := t0 + hVal
		if target <= t0 {
			continue
		}

		// Ensure j is strictly forward
		if j < i+1 {
			j = i + 1
		}
		for j < n && times[j] < target {
			j++
		}
		if j >= n {
			break // no more future horizon points
		}

		pEnd := prices[j]
		if pEnd <= 0 {
			continue
		}

		// simple return
		r := (pEnd - pStart) / pStart

		vSig = append(vSig, s)
		vRet = append(vRet, r)
	}

	*scratchSig = vSig
	*scratchRet = vRet

	if len(vSig) < 2 {
		return MetricStats{}
	}

	return ComputeStats(vSig, vRet)
}

// --- Helpers ---

func discoverStudyDays(vDir string) []int {
	var days []int
	files, _ := os.ReadDir(vDir)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".bin") {
			name := strings.TrimSuffix(f.Name(), ".bin")
			if len(name) == 8 {
				val := fastAtoi(name)
				if val > 0 {
					days = append(days, val)
				}
			}
		}
	}
	sort.Ints(days)
	return days
}

func printHorizonTable(
	sec int,
	variants []string,
	isAcc, oosAcc map[string][]Accumulator,
	hIdx int,
) {
	// Build results list for sorting
	type row struct {
		Name string
		IS   Accumulator
		OOS  Accumulator
	}
	var rows []row

	for _, v := range variants {
		rows = append(rows, row{
			Name: v,
			IS:   isAcc[v][hIdx],
			OOS:  oosAcc[v][hIdx],
		})
	}

	// Sort by OOS Sharpe Descending
	slices.SortFunc(rows, func(a, b row) int {
		sa := 0.0
		if a.OOS.Days > 0 {
			sa = a.OOS.SumSharpe / float64(a.OOS.Days)
		}
		sb := 0.0
		if b.OOS.Days > 0 {
			sb = b.OOS.SumSharpe / float64(b.OOS.Days)
		}
		if sa > sb {
			return -1
		}
		if sa < sb {
			return 1
		}
		return 0
	})

	fmt.Printf("== Horizon %d seconds ==\n", sec)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VARIANT\tIS_DAYS\tOOS_DAYS\tIS_IC\tOOS_IC\tIS_SR\tOOS_SR\tIS_HIT\tOOS_HIT\tIS_BE\tOOS_BE")
	fmt.Fprintln(w, "-------\t-------\t--------\t-----\t------\t-----\t------\t------\t-------\t-----\t------")

	for _, r := range rows {
		var isIC, isSR, isHit, isBE float64
		var oosIC, oosSR, oosHit, oosBE float64

		if r.IS.Days > 0 {
			div := float64(r.IS.Days)
			isIC = r.IS.SumIC / div
			isSR = r.IS.SumSharpe / div
			isHit = (r.IS.SumHit / div) * 100
			isBE = r.IS.SumBE / div
		}
		if r.OOS.Days > 0 {
			div := float64(r.OOS.Days)
			oosIC = r.OOS.SumIC / div
			oosSR = r.OOS.SumSharpe / div
			oosHit = (r.OOS.SumHit / div) * 100
			oosBE = r.OOS.SumBE / div
		}

		fmt.Fprintf(w, "%s\t%d\t%d\t%.4f\t%.4f\t%.2f\t%.2f\t%.1f%%\t%.1f%%\t%.1f\t%.1f\n",
			r.Name, r.IS.Days, r.OOS.Days,
			isIC, oosIC, isSR, oosSR, isHit, oosHit, isBE, oosBE,
		)
	}
	_ = w.Flush()
}

func parseOOSBoundary(dateStr string) int {
	if len(dateStr) < 10 {
		return 0
	}
	y := fastAtoi(dateStr[0:4])
	m := fastAtoi(dateStr[5:7])
	d := fastAtoi(dateStr[8:10])
	return y*10000 + m*100 + d
}

// fastAtoi converts an ASCII digit string (no sign) to int.
func fastAtoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
