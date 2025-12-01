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
	"sort"
	"strconv"
	"sync"
	"time"
)

// --- Configuration ----------------------------------------------------------

const (
	OFIExecutionLagMS = 15   // execution/network lag in milliseconds
	OFIWarmupTrades   = 2000 // ignore first N trades for metrics
)

// Horizons to test (in ticks after entry)
var OFIHorizons = []int{20, 50, 100}

// --- Task ID ---------------------------------------------------------------

type ofiTask struct {
	Y, M, D int
}

// --- Variant Configuration --------------------------------------------------

type OFIVariant struct {
	ID           string  // label: "OFI_VolumeTime_50M", etc.
	Kind         string  // "voltime", "invariant", "depth", "cluster", "raw"
	Alpha        float64 // EMA alpha (for non-voltime variants)
	DollarLambda float64 // for VolumeTime variants: exp(-dollar / lambda), 0 if unused
	NThreshold   int     // threshold on n_t (trade count), e.g. 13
	NBoost       float64 // depth / cluster weight multiplier
	UseDepth     bool    // multiply flow by n_t
	ZWindow      int     // rolling z-score window; 0 = off
	ResetEvery   int     // reset OFI every N trades (event-time); 0 = off
}

// Complete list of OFI variants to test.
var ofiVariants = []OFIVariant{
	{
		ID:           "OFI_VolumeTime_50M",
		Kind:         "voltime",
		DollarLambda: 50_000_000,
	},
	{
		ID:           "OFI_VolumeTime_30M",
		Kind:         "voltime",
		DollarLambda: 30_000_000,
	},
	{
		ID:         "OFI_Invariant_2025_Slow",
		Kind:       "invariant",
		Alpha:      0.018,
		NThreshold: 13,
		NBoost:     8.5,
		UseDepth:   true,
	},
	{
		ID:         "OFI_Invariant_2025_Fast",
		Kind:       "invariant",
		Alpha:      0.052,
		NThreshold: 13,
		NBoost:     8.5,
		UseDepth:   true,
	},
	{
		ID:       "OFI_ZScore_DepthWeighted_20k",
		Kind:     "depth",
		Alpha:    0.04,
		UseDepth: true,
		ZWindow:  20_000,
	},
	{
		ID:         "OFI_EventTime_2000",
		Kind:       "depth",
		Alpha:      0.08,
		UseDepth:   true,
		ResetEvery: 2_000,
	},
	{
		ID:       "OFI_DepthWeighted_SlowEMA",
		Kind:     "depth",
		Alpha:    0.015,
		UseDepth: true,
	},
	{
		ID:         "OFI_ClusterBoost_9_5x",
		Kind:       "cluster",
		Alpha:      0.05,
		NThreshold: 13,
		NBoost:     9.5,
	},
	{
		ID:      "OFI_ZScore10k_Raw_Slow",
		Kind:    "raw",
		Alpha:   0.02,
		ZWindow: 10_000,
	},
}

// --- OFI State --------------------------------------------------------------

type ofiState struct {
	cfg *OFIVariant

	ofi        float64
	tradeCount int // for ResetEvery

	// rolling z-score state
	zBuf   []float64
	zHead  int
	zCount int
	zSum   float64
	zSumSq float64
}

func newOFIState(cfg *OFIVariant) *ofiState {
	return &ofiState{
		cfg: cfg,
	}
}

func (s *ofiState) updateZScore(value float64) float64 {
	w := s.cfg.ZWindow
	if w <= 0 {
		return value
	}
	if s.zBuf == nil {
		s.zBuf = make([]float64, w)
	}

	if s.zCount < w {
		// still filling window
		s.zBuf[s.zHead] = value
		s.zSum += value
		s.zSumSq += value * value
		s.zCount++
		s.zHead++
		if s.zHead == w {
			s.zHead = 0
		}
	} else {
		// full window: replace oldest
		old := s.zBuf[s.zHead]
		s.zBuf[s.zHead] = value
		s.zSum += value - old
		s.zSumSq += value*value - old*old
		s.zHead++
		if s.zHead == w {
			s.zHead = 0
		}
	}

	if s.zCount < 2 {
		return 0
	}
	mean := s.zSum / float64(s.zCount)
	variance := s.zSumSq/float64(s.zCount) - mean*mean
	if variance <= 0 {
		return 0
	}
	return (value - mean) / math.Sqrt(variance)
}

// Update computes the OFI signal for a single tick.
func (s *ofiState) Update(px, qty float64, nTrades int, sign float64) float64 {
	cfg := s.cfg

	// base signed flow
	flow := sign * qty
	if cfg.UseDepth {
		flow *= float64(nTrades)
	}

	switch cfg.Kind {
	case "voltime":
		// OFI_t = s_t q_t p_t + exp(- q_t p_t / Lambda) * OFI_{t-1}
		dollar := qty * px
		decay := math.Exp(-dollar / cfg.DollarLambda)
		s.ofi = sign*qty*px + decay*s.ofi

	case "invariant":
		// f_t = s q w(n); w boosted above threshold
		w := float64(nTrades)
		if nTrades >= cfg.NThreshold {
			w *= cfg.NBoost
		}
		f := sign * qty * w
		alpha := cfg.Alpha
		s.ofi = alpha*f + (1.0-alpha)*s.ofi

	case "cluster":
		// w_t = { NBoost if n >= threshold; 1 else }
		w := 1.0
		if nTrades >= cfg.NThreshold {
			w = cfg.NBoost
		}
		f := sign * qty * w
		alpha := cfg.Alpha
		s.ofi = alpha*f + (1.0-alpha)*s.ofi

	case "depth", "raw":
		// f is flow or depth-weighted flow
		alpha := cfg.Alpha
		s.ofi = alpha*flow + (1.0-alpha)*s.ofi

	default:
		// fallback: raw EMA on flow
		alpha := cfg.Alpha
		s.ofi = alpha*flow + (1.0-alpha)*s.ofi
	}

	s.tradeCount++
	if cfg.ResetEvery > 0 && s.tradeCount >= cfg.ResetEvery {
		s.ofi = 0
		s.tradeCount = 0
	}

	value := s.ofi
	if cfg.ZWindow > 0 {
		value = s.updateZScore(value)
	}
	return value
}

// --- Tick Series Loader -----------------------------------------------------

// loadDayTicks loads one day from data.quantdev for (y,m,d) and returns
// SoA arrays: timestamps, prices, qty, sign, nTrades.
//
// sign = +1 for taker buy, -1 for taker sell
func loadDayTicks(y, m, d int) (ts []int64, px, qty, sign []float64, nTrades []int, ok bool) {
	dir := filepath.Join(BaseDir, Symbol, fmt.Sprintf("%04d", y), fmt.Sprintf("%02d", m))
	idxPath := filepath.Join(dir, "index.quantdev")
	dataPath := filepath.Join(dir, "data.quantdev")

	offset, length := findBlobOFI(idxPath, d)
	if length == 0 {
		return nil, nil, nil, nil, nil, false
	}

	f, err := os.Open(dataPath)
	if err != nil {
		return nil, nil, nil, nil, nil, false
	}
	defer f.Close()

	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, nil, nil, nil, nil, false
	}

	compData := make([]byte, length)
	if _, err := io.ReadFull(f, compData); err != nil {
		return nil, nil, nil, nil, nil, false
	}

	r, err := zlib.NewReader(bytes.NewReader(compData))
	if err != nil {
		return nil, nil, nil, nil, nil, false
	}
	blob, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		return nil, nil, nil, nil, nil, false
	}

	if len(blob) < HeaderSize {
		return nil, nil, nil, nil, nil, false
	}

	rowCount := binary.LittleEndian.Uint64(blob[8:])
	body := blob[HeaderSize:]
	n := int(rowCount)

	ts = make([]int64, n)
	px = make([]float64, n)
	qty = make([]float64, n)
	sign = make([]float64, n)
	nTrades = make([]int, n)

	invPx := 1.0 / PxScale
	invQt := 1.0 / QtScale

	rowOff := 0
	for i := 0; i < n; i++ {
		pxRaw := binary.LittleEndian.Uint64(body[rowOff+8:])
		qtyRaw := binary.LittleEndian.Uint64(body[rowOff+16:])
		cntRaw := binary.LittleEndian.Uint32(body[rowOff+32:])
		flags := binary.LittleEndian.Uint16(body[rowOff+36:])
		tsRaw := binary.LittleEndian.Uint64(body[rowOff+38:])
		rowOff += RowSize

		px[i] = float64(pxRaw) * invPx
		qty[i] = float64(qtyRaw) * invQt
		nTrades[i] = int(cntRaw)
		ts[i] = int64(tsRaw)

		// flags bit 0: 0 => taker buy, 1 => taker sell (from fastZipToAgg3)
		if (flags & 1) == 0 {
			sign[i] = 1.0
		} else {
			sign[i] = -1.0
		}
	}

	return ts, px, qty, sign, nTrades, true
}

func findBlobOFI(idxPath string, day int) (uint64, uint64) {
	f, err := os.Open(idxPath)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	hdr := make([]byte, 16)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return 0, 0
	}
	if string(hdr[:4]) != IdxMagic {
		return 0, 0
	}
	count := binary.LittleEndian.Uint64(hdr[8:])

	row := make([]byte, 26)
	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(f, row); err != nil {
			return 0, 0
		}
		if int(binary.LittleEndian.Uint16(row[0:])) == day {
			return binary.LittleEndian.Uint64(row[2:]), binary.LittleEndian.Uint64(row[10:])
		}
	}
	return 0, 0
}

// --- Lag / Horizon Helpers --------------------------------------------------

func buildEntryIndex(ts []int64, lagMS int64) []int {
	n := len(ts)
	idx := make([]int, n)
	j := 0
	for i := 0; i < n; i++ {
		target := ts[i] + lagMS
		if j < i {
			j = i
		}
		for j < n && ts[j] < target {
			j++
		}
		if j >= n {
			idx[i] = -1
		} else {
			idx[i] = j
		}
	}
	return idx
}

type horizonData struct {
	ret   []float64
	valid []bool
}

func buildHorizonData(px []float64, entryIdx []int, horizon, warmup int) horizonData {
	n := len(px)
	ret := make([]float64, n)
	valid := make([]bool, n)

	for i := warmup; i < n; i++ {
		e := entryIdx[i]
		if e < 0 || e+horizon >= n {
			continue
		}
		pEntry := px[e]
		pExit := px[e+horizon]
		if pEntry <= 0 {
			continue
		}
		ret[i] = (pExit - pEntry) / pEntry
		valid[i] = true
	}

	return horizonData{
		ret:   ret,
		valid: valid,
	}
}

// computeSignalSeries runs one OFI variant over all ticks and returns the
// signal series (length n).
func computeSignalSeries(cfg *OFIVariant, px, qty []float64, nTrades []int, sign []float64) []float64 {
	n := len(px)
	out := make([]float64, n)
	state := newOFIState(cfg)
	for i := 0; i < n; i++ {
		out[i] = state.Update(px[i], qty[i], nTrades[i], sign[i])
	}
	return out
}

// medianDeltaMs computes median inter-trade time in milliseconds.
func medianDeltaMs(ts []int64) float64 {
	n := len(ts)
	if n < 2 {
		return 0
	}
	delta := make([]int64, n-1)
	for i := 1; i < n; i++ {
		dt := ts[i] - ts[i-1]
		if dt <= 0 {
			dt = 1
		}
		delta[i-1] = dt
	}
	sort.Slice(delta, func(i, j int) bool { return delta[i] < delta[j] })
	mid := (n - 1) / 2
	if (n-1)%2 == 1 {
		return float64(delta[mid]+delta[mid+1]) * 0.5
	}
	return float64(delta[mid])
}

// --- Core Day Processing ----------------------------------------------------

func processOFIDay(y, m, d int, horizons []int) []AlphaMetrics {
	ts, px, qty, sign, nTrades, ok := loadDayTicks(y, m, d)
	if !ok {
		return nil
	}
	n := len(px)
	if n == 0 {
		return nil
	}

	// require enough data after warmup and max horizon
	maxH := 0
	for _, h := range horizons {
		if h > maxH {
			maxH = h
		}
	}
	if n < OFIWarmupTrades+maxH+10 {
		return nil
	}

	entryIdx := buildEntryIndex(ts, int64(OFIExecutionLagMS))

	// precompute horizon returns
	hMap := make(map[int]horizonData, len(horizons))
	for _, h := range horizons {
		hMap[h] = buildHorizonData(px, entryIdx, h, OFIWarmupTrades)
	}

	medianMs := medianDeltaMs(ts)
	dateLabel := fmt.Sprintf("%04d-%02d-%02d", y, m, d)
	var results []AlphaMetrics

	for vIdx := range ofiVariants {
		variant := &ofiVariants[vIdx]

		sigSeries := computeSignalSeries(variant, px, qty, nTrades, sign)
		if len(sigSeries) == 0 {
			continue
		}

		if len(sigSeries) <= OFIWarmupTrades {
			continue
		}
		sigForMetrics := sigSeries[OFIWarmupTrades:]

		am := AlphaMetrics{
			Label:   fmt.Sprintf("%s|%s|%s", Symbol, variant.ID, dateLabel),
			NBars:   len(sigForMetrics),
			Signal:  ComputeSignalMetrics(sigForMetrics),
			Horizon: make(map[string]HorizonMetrics),
		}

		for _, h := range horizons {
			hd := hMap[h]
			var subSig, subRet []float64

			for i := OFIWarmupTrades; i < n; i++ {
				if !hd.valid[i] {
					continue
				}
				subSig = append(subSig, sigSeries[i])
				subRet = append(subRet, hd.ret[i])
			}

			if len(subSig) == 0 {
				continue
			}

			hm := ComputeHorizonMetrics(subSig, subRet)
			if hm.AlphaHalfLifeBars > 0 && medianMs > 0 {
				hm.AlphaHalfLifeMs = hm.AlphaHalfLifeBars * medianMs
			}
			am.Horizon[fmt.Sprintf("%d", h)] = hm
		}

		// Only keep if we have at least one horizon with metrics
		if len(am.Horizon) > 0 {
			results = append(results, am)
		}
	}

	return results
}

// --- Public Entry Point -----------------------------------------------------

func runOFI() {
	start := time.Now()
	root := filepath.Join(BaseDir, Symbol)

	fmt.Printf("--- OFI LAB | Symbol: %s | Lag: %dms | Horizons: %v ---\n",
		Symbol, OFIExecutionLagMS, OFIHorizons)

	years, err := os.ReadDir(root)
	if err != nil {
		fmt.Printf("[ofi] cannot read root %s: %v\n", root, err)
		return
	}

	var tasks []ofiTask
	for _, yDir := range years {
		if !yDir.IsDir() {
			continue
		}
		y, err := strconv.Atoi(yDir.Name())
		if err != nil {
			continue
		}
		months, err := os.ReadDir(filepath.Join(root, yDir.Name()))
		if err != nil {
			continue
		}
		for _, mDir := range months {
			if !mDir.IsDir() {
				continue
			}
			m, err := strconv.Atoi(mDir.Name())
			if err != nil {
				continue
			}
			idxPath := filepath.Join(root, yDir.Name(), mDir.Name(), "index.quantdev")
			if _, err := os.Stat(idxPath); err != nil {
				continue
			}
			for d := 1; d <= 31; d++ {
				tasks = append(tasks, ofiTask{Y: y, M: m, D: d})
			}
		}
	}

	fmt.Printf("[ofi] Scanning %d potential (year,month,day) slices using %d threads...\n",
		len(tasks), CPUThreads)

	jobs := make(chan ofiTask, len(tasks))
	results := make(chan []AlphaMetrics, len(tasks))
	var wg sync.WaitGroup

	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				ms := processOFIDay(t.Y, t.M, t.D, OFIHorizons)
				if len(ms) > 0 {
					results <- ms
				}
			}
		}()
	}

	for _, t := range tasks {
		jobs <- t
	}
	close(jobs)
	wg.Wait()
	close(results)

	var all []AlphaMetrics
	for ms := range results {
		all = append(all, ms...)
	}

	if len(all) == 0 {
		fmt.Println("[ofi] No metrics produced.")
		return
	}

	// Deterministic ordering by label
	sort.Slice(all, func(i, j int) bool { return all[i].Label < all[j].Label })

	outPath := filepath.Join(BaseDir, "reports", fmt.Sprintf("ofi_%s.json", Symbol))
	if err := SaveAlphaMetrics(outPath, all); err != nil {
		fmt.Printf("[ofi] error saving metrics: %v\n", err)
	} else {
		fmt.Println("[ofi] metrics saved:", outPath)
	}

	fmt.Printf("[ofi] total records: %d | elapsed: %s\n", len(all), time.Since(start))
}
