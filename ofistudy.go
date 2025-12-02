package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"iter"
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

// --- Configuration ---

const (
	OOSDateStr   = "2024-01-01"
	StudyMaxRows = 10_000_000
	NumBuckets   = 5
)

var TimeHorizonsSec = []int{10, 30, 60, 180, 300}
var oosBoundaryYMD int

func init() {
	oosBoundaryYMD = parseOOSBoundary(OOSDateStr)
}

type DayResult struct {
	YMD int
	// Metrics[VariantKey][HorizonIdx]
	Metrics map[string][]Moments
	// Quantiles[VariantKey][HorizonIdx] -> []BucketResult
	Quantiles map[string]map[int][]BucketResult
}

// --- Main entrypoint ---

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
	fmt.Printf("Variants: %d | Days: %d\n", len(variants), len(tasks))

	// Aggregators
	// Map[FeatureKey][HorizonIdx] -> Moments
	isAcc := make(map[string][]Moments)
	oosAcc := make(map[string][]Moments)

	// Map[FeatureKey][HorizonIdx] -> []DailyIC
	isDailyIC := make(map[string]map[int][]float64)
	oosDailyIC := make(map[string]map[int][]float64)

	// Map[FeatureKey][HorizonIdx][BucketIdx] -> BucketAgg
	isBuckets := make(map[string]map[int][]BucketAgg)
	oosBuckets := make(map[string]map[int][]BucketAgg)

	var accMu sync.Mutex

	resultsChan := make(chan DayResult, 64)
	jobsChan := make(chan int, len(tasks))
	var wg sync.WaitGroup

	// Workers
	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Per-worker reusable buffers
			prices := make([]float64, StudyMaxRows)
			times := make([]int64, StudyMaxRows)
			sigBuf := make([]float64, StudyMaxRows*5) // up to 5D
			fileBuf := make([]byte, StudyMaxRows*5*8)

			for idx := range jobsChan {
				res := processStudyDay(
					sym, tasks[idx],
					variants, featRoot,
					&prices, &times,
					&sigBuf, &fileBuf,
				)
				resultsChan <- res
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
			// Initialize maps on first sight of this feature key
			if _, ok := isAcc[vName]; !ok {
				isAcc[vName] = make([]Moments, len(TimeHorizonsSec))
				oosAcc[vName] = make([]Moments, len(TimeHorizonsSec))
				isDailyIC[vName] = make(map[int][]float64)
				oosDailyIC[vName] = make(map[int][]float64)
				isBuckets[vName] = make(map[int][]BucketAgg)
				oosBuckets[vName] = make(map[int][]BucketAgg)
			}

			// Select which side (IS vs OOS) we are aggregating into.
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

				// Aggregate moments
				tMoments[hIdx].Add(m)

				// Per-day IC for t-stat
				num := m.Count*m.SumProd - m.SumSig*m.SumRet
				denX := m.Count*m.SumSqSig - m.SumSig*m.SumSig
				denY := m.Count*m.SumSqRet - m.SumRet*m.SumRet
				den := denX * denY
				ic := 0.0
				if den > 0 {
					ic = num / math.Sqrt(den)
				}
				tDailyIC[hIdx] = append(tDailyIC[hIdx], ic)

				// Aggregate quantile buckets, if present for this day
				if res.Quantiles != nil {
					if qByH, ok := res.Quantiles[vName]; ok {
						if qList, ok2 := qByH[hIdx]; ok2 && len(qList) > 0 {
							if len(tBuckets[hIdx]) == 0 {
								tBuckets[hIdx] = make([]BucketAgg, NumBuckets)
							}
							for i, br := range qList {
								if i >= NumBuckets {
									break
								}
								tBuckets[hIdx][i].Add(br)
							}
						}
					}
				}
			}
		}
		accMu.Unlock()
	}

	// Collect ordered feature keys
	var finalKeys []string
	for k := range isAcc {
		finalKeys = append(finalKeys, k)
	}
	sort.Strings(finalKeys)

	for hIdx, sec := range TimeHorizonsSec {
		printHorizonTable(sec, finalKeys, isAcc, oosAcc, isDailyIC, oosDailyIC, hIdx, isDays, oosDays)
		printMonotonicityTable(sec, finalKeys, isBuckets, hIdx)
		fmt.Println()
	}
}

// --- Per-day processing ---

func processStudyDay(
	sym string,
	dayInt int,
	variants []string,
	featRoot string,
	prices *[]float64,
	times *[]int64,
	sigBuf *[]float64,
	fileBuf *[]byte,
) DayResult {
	y, m, d := dayInt/10000, (dayInt%10000)/100, dayInt%100

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

	// Ensure buffers
	if n > cap(*prices) {
		*prices = make([]float64, n+n/4)
	}
	if n > cap(*times) {
		*times = make([]int64, n+n/4)
	}
	p := (*prices)[:n]
	t := (*times)[:n]

	for i := 0; i < n; i++ {
		off := i * RowSize
		p[i] = float64(binary.LittleEndian.Uint64(rawBytes[off+8:]))
		t[i] = int64(binary.LittleEndian.Uint64(rawBytes[off+38:]))
	}

	dStr := fmt.Sprintf("%04d%02d%02d", y, m, d)

	// Per-day working slices for quantiles
	var qSigs, qRets []float64

	for _, v := range variants {
		sigPath := filepath.Join(featRoot, v, dStr+".bin")

		rawSigs, byteSize, ok := fastLoadBytes(sigPath, fileBuf)
		if !ok || n == 0 {
			continue
		}
		if byteSize%(n*8) != 0 {
			continue
		}
		dims := byteSize / (n * 8)
		if dims < 1 || dims > 5 {
			continue
		}

		if n > cap(*sigBuf) {
			*sigBuf = make([]float64, n+n/4)
		}

		featureNames := []string{"f1_Z", "f2_SFA", "f3_Elast", "f4_Coh", "f5_Align"}

		for dim := 0; dim < dims; dim++ {
			target := (*sigBuf)[:n]

			// De-interleave one dimension: contiguous view in target
			for i := 0; i < n; i++ {
				offset := (i*dims + dim) * 8
				bits := binary.LittleEndian.Uint64(rawSigs[offset:])
				target[i] = math.Float64frombits(bits)
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
			qMap := make(map[int][]BucketResult)

			for hIdx, sec := range TimeHorizonsSec {
				// Materialize aligned (sig, ret) pairs for this horizon
				// so we can compute Moments + Quantiles.
				if cap(qSigs) < n {
					qSigs = make([]float64, n)
				}
				if cap(qRets) < n {
					qRets = make([]float64, n)
				}
				k := 0

				seq := AlignVectors(target, p, t, sec*1000)
				for s, r := range seq {
					qSigs[k] = s
					qRets[k] = r
					k++
				}
				if k == 0 {
					continue
				}

				sliceS := qSigs[:k]
				sliceR := qRets[:k]

				// 1) Moments from vectors
				moms[hIdx] = CalcMomentsVectors(sliceS, sliceR)

				// 2) Quantile buckets (monotonicity)
				qMap[hIdx] = ComputeQuantiles(sliceS, sliceR, NumBuckets)
			}

			res.Metrics[key] = moms
			res.Quantiles[key] = qMap
		}
	}

	return res
}

// AlignVectors unchanged: streaming alignment in time.
func AlignVectors(sig, prices []float64, times []int64, hMs int) iter.Seq2[float64, float64] {
	return func(yield func(float64, float64) bool) {
		n := len(sig)
		j := 0
		hVal := int64(hMs)

		for i := 0; i < n; i++ {
			s := sig[i]
			if s == 0 {
				continue
			}
			pStart := prices[i]
			if pStart <= 0 {
				continue
			}

			tTarget := times[i] + hVal
			if j < i+1 {
				j = i + 1
			}

			for j < n && times[j] < tTarget {
				j++
			}
			if j >= n {
				break
			}

			pEnd := prices[j]
			if pEnd > 0 {
				r := (pEnd - pStart) / pStart
				if !yield(s, r) {
					return
				}
			}
		}
	}
}

// Loads feature file into shared buffer.
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

func printHorizonTable(
	sec int,
	keys []string,
	isAcc, oosAcc map[string][]Moments,
	isDailyIC, oosDailyIC map[string]map[int][]float64,
	hIdx, isDays, oosDays int,
) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "== Horizon %ds [IS: %d | OOS: %d] ==\n", sec, isDays, oosDays)
	fmt.Fprintln(w, "FEATURE\tIS_IC\tIS_T\tOOS_IC\tOOS_T\tAC(Lag1)\tBPS/TR")

	for _, k := range keys {
		var isICSlice, oosICSlice []float64
		if m, ok := isDailyIC[k]; ok {
			isICSlice = m[hIdx]
		}
		if m, ok := oosDailyIC[k]; ok {
			oosICSlice = m[hIdx]
		}

		is := FinalizeMetrics(isAcc[k][hIdx], isICSlice)
		oos := FinalizeMetrics(oosAcc[k][hIdx], oosICSlice)

		fmt.Fprintf(w, "%s\t%.4f\t%.2f\t%.4f\t%.2f\t%.3f\t%.2f\n",
			k,
			is.ICPearson, is.IC_TStat,
			oos.ICPearson, oos.IC_TStat,
			is.AutoCorr,
			is.BreakevenBps,
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
	fmt.Fprintln(w, "FEATURE\tB1(Sell)\tB2\tB3\tB4\tB5(Buy)")

	for _, k := range keys {
		bAggsByH, ok := isBuckets[k]
		if !ok {
			continue
		}
		aggs := bAggsByH[hIdx]
		if len(aggs) < NumBuckets {
			continue
		}

		fmt.Fprintf(w, "%s", k)
		for i := 0; i < NumBuckets; i++ {
			res := aggs[i].Finalize(i + 1)
			fmt.Fprintf(w, "\t%.1f", res.AvgRetBps)
		}
		fmt.Fprintln(w)
	}
	w.Flush()
}

// --- Index / raw loader helpers ---

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
	if len(raw) < HeaderSize {
		return nil, 0, false
	}
	rowCount := binary.LittleEndian.Uint64(raw[8:])
	return raw[HeaderSize:], rowCount, true
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
