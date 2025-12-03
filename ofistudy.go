package main

import (
	"bytes"
	"compress/zlib"
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
	"sync/atomic"
	"text/tabwriter"
	"time"
)

// --- Configuration ---

const (
	OOSDateStr     = "2024-01-01"
	StudyMaxRows   = 10_000_000
	NumBuckets     = 5
	QuantileStride = 10 // Only use 1 in 10 rows for quantile sorting (10x speedup)
)

var TimeHorizonsSec = []int{10, 30, 60, 180, 300}

var oosBoundaryYMD int

func init() {
	oosBoundaryYMD = parseOOSBoundary(OOSDateStr)
}

// DayResult carries per-day metrics and quantile results
type DayResult struct {
	YMD int
	// Metrics[VariantKey][HorizonIdx]
	Metrics map[string][]Moments
	// Quantiles[VariantKey][HorizonIdx] -> []BucketResult
	Quantiles map[string]map[int][]BucketResult
}

// --- Main Logic ---

func runStudy() {
	startT := time.Now()

	symbols := discoverFeatureSymbols()
	fmt.Printf("--- STUDY | Found Feature Sets: %v ---\n", symbols)

	for _, sym := range symbols {
		studySymbol(sym)
	}

	fmt.Printf("[study] ALL COMPLETE in %s\n", time.Since(startT))
}

func discoverFeatureSymbols() []string {
	var syms []string
	featDir := filepath.Join(BaseDir, "features")
	entries, err := os.ReadDir(featDir)
	if err != nil {
		return syms
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			syms = append(syms, e.Name())
		}
	}
	return syms
}

func studySymbol(sym string) {
	fmt.Printf("\n>>> STUDY: %s <<<\n", sym)
	featRoot := filepath.Join(BaseDir, "features", sym)

	entries, err := os.ReadDir(featRoot)
	if err != nil {
		return
	}
	var variants []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			variants = append(variants, e.Name())
		}
	}
	slices.Sort(variants)
	if len(variants) == 0 {
		return
	}

	tasks := discoverStudyDays(filepath.Join(featRoot, variants[0]))
	totalTasks := len(tasks)
	fmt.Printf("Variants: %d | Days: %d\n", len(variants), totalTasks)

	// Aggregators
	isAcc := make(map[string][]Moments)
	oosAcc := make(map[string][]Moments)
	isDailyIC := make(map[string]map[int][]float64)
	oosDailyIC := make(map[string]map[int][]float64)
	isBuckets := make(map[string]map[int][]BucketAgg)
	oosBuckets := make(map[string]map[int][]BucketAgg)

	var accMu sync.Mutex

	resultsChan := make(chan DayResult, 64)
	jobsChan := make(chan int, len(tasks))
	var wg sync.WaitGroup

	// --- Progress Bar State ---
	var completed atomic.Int64
	doneChan := make(chan bool)

	// Monitor Goroutine
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		start := time.Now()

		for {
			select {
			case <-doneChan:
				printProgress(totalTasks, totalTasks, start)
				fmt.Println()
				return
			case <-ticker.C:
				curr := completed.Load()
				printProgress(int(curr), totalTasks, start)
			}
		}
	}()

	// Workers
	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Thread-local buffers - Optimized for RAM
			// MaxRows = 10M. float64 = 8 bytes.
			// prices: 80MB
			prices := make([]float64, StudyMaxRows)
			// times: 80MB
			times := make([]int64, StudyMaxRows)

			// sigBuf: 80MB (process 1 feature dim at a time)
			sigBuf := make([]float64, StudyMaxRows)

			// fileBuf: raw feature binary (float32 interleaved)
			fileBuf := make([]byte, StudyMaxRows*FeatDims*FeatBytes)

			// retBuf: 80MB (per-horizon reused)
			retBuf := make([]float64, StudyMaxRows)

			for idx := range jobsChan {
				dayInt := tasks[idx]
				// Quantiles only for IS (pre-OOS boundary)
				doQuantiles := dayInt < oosBoundaryYMD

				res := processStudyDay(
					sym,
					dayInt,
					variants,
					featRoot,
					&prices,
					&times,
					&sigBuf,
					&fileBuf,
					&retBuf,
					doQuantiles,
				)
				resultsChan <- res
				completed.Add(1)
			}
		}()
	}

	for i := range tasks {
		jobsChan <- i
	}
	close(jobsChan)

	go func() {
		wg.Wait()
		close(resultsChan)
		close(doneChan)
	}()

	isDays, oosDays := 0, 0

	for res := range resultsChan {
		if len(res.Metrics) == 0 {
			continue
		}
		isOOS := res.YMD >= oosBoundaryYMD
		if isOOS {
			oosDays++
		} else {
			isDays++
		}

		accMu.Lock()
		for vName, moms := range res.Metrics {
			if _, ok := isAcc[vName]; !ok {
				isAcc[vName] = make([]Moments, len(TimeHorizonsSec))
				oosAcc[vName] = make([]Moments, len(TimeHorizonsSec))
				isDailyIC[vName] = make(map[int][]float64)
				oosDailyIC[vName] = make(map[int][]float64)
				isBuckets[vName] = make(map[int][]BucketAgg)
				oosBuckets[vName] = make(map[int][]BucketAgg)
			}

			tMoments := isAcc[vName]
			tDailyIC := isDailyIC[vName]
			tBuckets := isBuckets[vName]
			if isOOS {
				tMoments = oosAcc[vName]
				tDailyIC = oosDailyIC[vName]
				tBuckets = oosBuckets[vName]
			}

			for hIdx := range TimeHorizonsSec {
				m := moms[hIdx]
				if m.Count <= 0 {
					continue
				}

				tMoments[hIdx].Add(m)

				num := m.Count*m.SumProd - m.SumSig*m.SumRet
				denX := m.Count*m.SumSqSig - m.SumSig*m.SumSig
				denY := m.Count*m.SumSqRet - m.SumRet*m.SumRet
				den := denX * denY
				ic := 0.0
				if den > 0 {
					ic = num / math.Sqrt(den)
				}
				tDailyIC[hIdx] = append(tDailyIC[hIdx], ic)

				if qMap, ok := res.Quantiles[vName]; ok {
					if qList, ok2 := qMap[hIdx]; ok2 {
						if len(tBuckets[hIdx]) == 0 {
							tBuckets[hIdx] = make([]BucketAgg, NumBuckets)
						}
						for i, bucket := range qList {
							if i < NumBuckets {
								tBuckets[hIdx][i].Add(bucket)
							}
						}
					}
				}
			}
		}
		accMu.Unlock()
	}

	// Output Tables
	var finalKeys []string
	for k := range isAcc {
		finalKeys = append(finalKeys, k)
	}
	sort.Strings(finalKeys)

	for hIdx, sec := range TimeHorizonsSec {
		printHorizonTable(
			sec,
			finalKeys,
			isAcc,
			oosAcc,
			isDailyIC,
			oosDailyIC,
			hIdx,
			isDays,
			oosDays,
		)
		printMonotonicityTable(
			sec,
			finalKeys,
			isBuckets,
			hIdx,
		)
		fmt.Println()
	}
}

// --- Progress Helper ---

func printProgress(curr, total int, start time.Time) {
	if total == 0 {
		return
	}
	const barWidth = 40
	percent := float64(curr) / float64(total)
	if percent > 1.0 {
		percent = 1.0
	}

	filled := int(percent * float64(barWidth))
	empty := barWidth - filled

	bar := strings.Repeat("=", filled) + strings.Repeat("-", empty)
	if filled > 0 && filled < barWidth {
		bar = bar[:filled-1] + ">" + bar[filled:]
	}

	elapsed := time.Since(start).Seconds()
	rate := 0.0
	if elapsed > 0 {
		rate = float64(curr) / elapsed
	}

	fmt.Printf("\r[%s] %.1f%% (%d/%d) | %.1f days/s  ", bar, percent*100, curr, total, rate)
}

func processStudyDay(
	sym string,
	dayInt int,
	variants []string,
	featRoot string,
	prices *[]float64,
	times *[]int64,
	sigBuf *[]float64,
	fileBuf *[]byte,
	retBuf *[]float64, // Pre-allocated return buffer (sized for 1 horizon)
	doQuantiles bool, // Only IS
) DayResult {
	y := dayInt / 10000
	m := (dayInt % 10000) / 100
	d := dayInt % 100

	res := DayResult{
		YMD:       dayInt,
		Metrics:   make(map[string][]Moments),
		Quantiles: make(map[string]map[int][]BucketResult),
	}

	rawBytes, rowCount, ok := loadRawDay(sym, y, m, d)
	if !ok || rowCount == 0 {
		return res
	}
	n := int(rowCount)

	if n > cap(*prices) {
		*prices = make([]float64, n+n/4)
	}
	if n > cap(*times) {
		*times = make([]int64, n+n/4)
	}
	p := (*prices)[:n]
	tm := (*times)[:n]

	for i := 0; i < n; i++ {
		off := i * RowSize
		p[i] = float64(binary.LittleEndian.Uint64(rawBytes[off+8:]))
		tm[i] = int64(binary.LittleEndian.Uint64(rawBytes[off+38:]))
	}

	dStr := fmt.Sprintf("%04d%02d%02d", y, m, d)

	// --- Feature Loop ---

	for _, v := range variants {
		sigPath := filepath.Join(featRoot, v, dStr+".bin")

		rawSigs, byteSize, ok := fastLoadBytes(sigPath, fileBuf)
		if !ok || byteSize == 0 {
			continue
		}

		if byteSize%(n*FeatBytes) != 0 {
			continue
		}
		dims := byteSize / (n * FeatBytes)
		if dims < 1 || dims > FeatDims {
			continue
		}

		if n > cap(*sigBuf) {
			*sigBuf = make([]float64, n+n/4)
		}

		featureNames := []string{"f1_Z", "f2_SFA", "f3_Elast", "f4_Coh", "f5_Align"}

		for dim := 0; dim < dims; dim++ {
			target := (*sigBuf)[:n]

			// De-interleave float32 -> float64
			for i := 0; i < n; i++ {
				offset := (i*dims + dim) * FeatBytes
				bits := binary.LittleEndian.Uint32(rawSigs[offset:])
				target[i] = float64(math.Float32frombits(bits))
			}

			key := v
			if dims > 1 {
				suffix := fmt.Sprintf("_d%d", dim+1)
				if dim < len(featureNames) {
					suffix = "_" + featureNames[dim]
				}
				key = v + suffix
			}

			moms := make([]Moments, len(TimeHorizonsSec))
			var qMap map[int][]BucketResult
			if doQuantiles {
				qMap = make(map[int][]BucketResult)
			}

			// JIT Return Calculation Loop (per horizon)
			for hIdx, sec := range TimeHorizonsSec {
				// 1. Calculate returns for this horizon ONLY
				computeReturns(p, tm, n, sec, retBuf)
				rets := (*retBuf)[:n]

				// 2. Calc Stats (Moments + BPS/TR)
				moms[hIdx] = CalcMomentsDirect(target, rets)

				if doQuantiles {
					qMap[hIdx] = ComputeQuantilesStrided(target, rets, NumBuckets, QuantileStride)
				}
			}

			res.Metrics[key] = moms
			if doQuantiles && len(qMap) > 0 {
				res.Quantiles[key] = qMap
			}
		}
	}

	return res
}

// computeReturns calculates future returns for a specific horizon into outBuf
func computeReturns(p []float64, tm []int64, n int, sec int, outBuf *[]float64) {
	if n > cap(*outBuf) {
		*outBuf = make([]float64, n+n/4)
	}
	outSlice := (*outBuf)[:n]

	hVal := int64(sec * 1000)
	right := 0

	for left := 0; left < n; left++ {
		targetTime := tm[left] + hVal

		if right < left {
			right = left
		}
		for right < n && tm[right] < targetTime {
			right++
		}

		if right >= n {
			// End of data: fill remainder with 0
			for k := left; k < n; k++ {
				outSlice[k] = 0
			}
			return
		}

		pStart := p[left]
		pEnd := p[right]
		if pStart > 0 {
			outSlice[left] = (pEnd - pStart) / pStart
		} else {
			outSlice[left] = 0
		}
	}
}

// CalcMomentsDirect computes moments without re-aligning
func CalcMomentsDirect(sigs, rets []float64) Moments {
	var m Moments
	n := len(sigs)

	var prevSig float64
	var prevSign float64
	var curSegLen float64

	for i := 0; i < n; i++ {
		s := sigs[i]
		r := rets[i]

		m.Count++
		m.SumSig += s
		m.SumRet += r
		m.SumSqSig += s * s
		m.SumSqRet += r * r
		m.SumProd += s * r

		pnl := s * r
		m.SumPnL += pnl
		m.SumSqPnL += pnl * pnl

		absS := s
		if absS < 0 {
			absS = -absS
		}
		m.SumAbsSig += absS

		if s != 0 && r != 0 {
			m.ValidHits++
			if (s > 0 && r > 0) || (s < 0 && r < 0) {
				m.Hits++
			}
		}

		// Dynamics
		if i > 0 {
			d := s - prevSig
			if d < 0 {
				d = -d
			}
			m.SumAbsDeltaSig += d
			m.SumProdLag += s * prevSig

			absPrev := prevSig
			if absPrev < 0 {
				absPrev = -absPrev
			}
			m.SumAbsProdLag += absS * absPrev
		}

		// Segment logic
		sign := 0.0
		if s > 0 {
			sign = 1.0
		} else if s < 0 {
			sign = -1.0
		}

		if sign != 0 {
			if prevSign == sign {
				curSegLen++
			} else {
				if curSegLen > 0 {
					m.SegCount++
					m.SegLenTotal += curSegLen
					if curSegLen > m.SegLenMax {
						m.SegLenMax = curSegLen
					}
				}
				curSegLen = 1
			}
		} else {
			if curSegLen > 0 {
				m.SegCount++
				m.SegLenTotal += curSegLen
				if curSegLen > m.SegLenMax {
					m.SegLenMax = curSegLen
				}
				curSegLen = 0
			}
		}

		prevSig = s
		prevSign = sign
	}

	if curSegLen > 0 {
		m.SegCount++
		m.SegLenTotal += curSegLen
		if curSegLen > m.SegLenMax {
			m.SegLenMax = curSegLen
		}
	}

	return m
}

// ComputeQuantilesStrided sorts a SUBSET of data (stride) to find buckets fast.
func ComputeQuantilesStrided(sigs, rets []float64, numBuckets, stride int) []BucketResult {
	n := len(sigs)
	if n == 0 || numBuckets <= 0 {
		return nil
	}

	estSize := n / stride
	type pair struct{ s, r float64 }
	pairs := make([]pair, 0, estSize)

	for i := 0; i < n; i += stride {
		pairs = append(pairs, pair{s: sigs[i], r: rets[i]})
	}

	if len(pairs) == 0 {
		return nil
	}

	slices.SortFunc(pairs, func(a, b pair) int {
		if a.s < b.s {
			return -1
		}
		if a.s > b.s {
			return 1
		}
		return 0
	})

	subN := len(pairs)
	results := make([]BucketResult, numBuckets)
	bucketSize := subN / numBuckets
	if bucketSize == 0 {
		bucketSize = 1
	}

	for b := 0; b < numBuckets; b++ {
		start := b * bucketSize
		end := start + bucketSize
		if b == numBuckets-1 || end > subN {
			end = subN
		}

		var sumS, sumR float64
		count := 0
		for i := start; i < end; i++ {
			sumS += pairs[i].s
			sumR += pairs[i].r
			count++
		}
		if count > 0 {
			results[b] = BucketResult{
				ID:        b + 1,
				AvgSig:    sumS / float64(count),
				AvgRetBps: (sumR / float64(count)) * 10000.0,
				Count:     count * stride,
			}
		}
	}
	return results
}

func fastLoadBytes(path string, fileBuf *[]byte) ([]byte, int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, 0, false
	}
	size := int(fi.Size())
	if size == 0 {
		return nil, 0, false
	}

	if cap(*fileBuf) < size {
		*fileBuf = make([]byte, size)
	}
	buf := (*fileBuf)[:size]

	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, 0, false
	}
	return buf, size, true
}

// --- Output ---

// printHorizonTable now reports BOTH IS and OOS BPS/TR.
//
// Interpretation of BPS/TR:
//
//   - This is BreakevenBps from MetricStats.
//
//   - It is GROSS alpha at mid per 1 unit of notional traded (per side).
//
//   - For a pure taker strategy with fee F bps PER SIDE,
//     NetBpsPerSide = OOS_BPS_TR - F
//
//   - So:
//     OOS_BPS_TR > 4  → can in principle beat 4 bps per-side taker fee.
//     OOS_BPS_TR > 2  → can beat 2 bps effective per-side maker/taker blend.
func printHorizonTable(
	sec int,
	keys []string,
	isAcc, oosAcc map[string][]Moments,
	isDailyIC, oosDailyIC map[string]map[int][]float64,
	hIdx, isDays, oosDays int,
) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "== Horizon %ds [IS: %d | OOS: %d] ==\n", sec, isDays, oosDays)
	fmt.Fprintln(w, "FEATURE\tIS_IC\tIS_T\tOOS_IC\tOOS_T\tAC1\t|AC1|\tAVG_SEG\tMAX_SEG\tIS_BPS/TR\tOOS_BPS/TR")

	for _, k := range keys {
		var isICSlice, oosICSlice []float64
		if m, ok := isDailyIC[k]; ok {
			isICSlice = m[hIdx]
		}
		if m, ok := oosDailyIC[k]; ok {
			oosICSlice = m[hIdx]
		}

		isStats := FinalizeMetrics(isAcc[k][hIdx], isICSlice)
		oosStats := FinalizeMetrics(oosAcc[k][hIdx], oosICSlice)

		fmt.Fprintf(w, "%s\t%.4f\t%.2f\t%.4f\t%.2f\t%.3f\t%.3f\t%.2f\t%.1f\t%.2f\t%.2f\n",
			k,
			isStats.ICPearson, isStats.IC_TStat,
			oosStats.ICPearson, oosStats.IC_TStat,
			isStats.AutoCorr,
			isStats.AutoCorrAbs,
			isStats.AvgSegLen,
			isStats.MaxSegLen,
			isStats.BreakevenBps,  // IS gross bps per side-turnover
			oosStats.BreakevenBps, // OOS gross bps per side-turnover (use this vs fees)
		)
	}
	w.Flush()
}

func printMonotonicityTable(
	sec int,
	keys []string,
	isBuckets map[string]map[int][]BucketAgg,
	hIdx int,
) {
	fmt.Printf("\n-- Monotonicity Check (IS) Horizon %ds --\n", sec)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	fmt.Fprintln(w, "FEATURE\tMONO\tB1(Sell)\tB2\tB3\tB4\tB5(Buy)")

	for _, k := range keys {
		aggs, ok := isBuckets[k][hIdx]
		if !ok || len(aggs) < NumBuckets {
			continue
		}

		brets := make([]float64, NumBuckets)
		for i := 0; i < NumBuckets; i++ {
			br := aggs[i].Finalize(i + 1)
			brets[i] = br.AvgRetBps
		}

		mono := bucketMonotonicity(brets)

		fmt.Fprintf(w, "%s\t%.3f", k, mono)
		for i := 0; i < NumBuckets; i++ {
			fmt.Fprintf(w, "\t%.1f", brets[i])
		}
		fmt.Fprintln(w, "")
	}
	w.Flush()
}

func bucketMonotonicity(rets []float64) float64 {
	n := len(rets)
	if n == 0 {
		return 0
	}
	var sumX, sumY, sumXY, sumX2, sumY2 float64
	nf := float64(n)
	for i := 0; i < n; i++ {
		x := float64(i + 1)
		y := rets[i]
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
		sumY2 += y * y
	}
	num := nf*sumXY - sumX*sumY
	denX := nf*sumX2 - sumX*sumX
	denY := nf*sumY2 - sumY*sumY
	if denX <= 0 || denY <= 0 {
		return 0
	}
	return num / math.Sqrt(denX*denY)
}

func discoverStudyDays(vDir string) []int {
	var days []int
	files, _ := os.ReadDir(vDir)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".bin") {
			if val := fastAtoi(strings.TrimSuffix(f.Name(), ".bin")); val > 0 {
				days = append(days, val)
			}
		}
	}
	sort.Ints(days)
	return days
}

func parseOOSBoundary(d string) int {
	return fastAtoi(d[0:4])*10000 + fastAtoi(d[5:7])*100 + fastAtoi(d[8:10])
}

func fastAtoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

func loadRawDay(sym string, y, m, d int) ([]byte, uint64, bool) {
	dir := filepath.Join(BaseDir, sym, fmt.Sprintf("%04d", y), fmt.Sprintf("%02d", m))
	idxPath := filepath.Join(dir, "index.quantdev")
	dataPath := filepath.Join(dir, "data.quantdev")

	offset, length := findBlobOffset(idxPath, d)
	if length == 0 {
		return nil, 0, false
	}

	f, err := os.Open(dataPath)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()

	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, 0, false
	}
	compData := make([]byte, length)
	if _, err := io.ReadFull(f, compData); err != nil {
		return nil, 0, false
	}
	r, err := zlib.NewReader(bytes.NewReader(compData))
	if err != nil {
		return nil, 0, false
	}
	raw, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		return nil, 0, false
	}
	if len(raw) < 48 {
		return nil, 0, false
	}
	rowCount := binary.LittleEndian.Uint64(raw[8:])
	return raw[48:], rowCount, true
}

func findBlobOffset(idxPath string, day int) (uint64, uint64) {
	f, err := os.Open(idxPath)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	var hdr [16]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return 0, 0
	}
	count := binary.LittleEndian.Uint64(hdr[8:])
	var row [26]byte
	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(f, row[:]); err != nil {
			break
		}
		if int(binary.LittleEndian.Uint16(row[0:])) == day {
			return binary.LittleEndian.Uint64(row[2:]), binary.LittleEndian.Uint64(row[10:])
		}
	}
	return 0, 0
}
