// --- File: ofibuild.go ---
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
	"strconv"
	"strings"
	"sync"
	"time"
)

const BuildMaxRows = 10_000_000

// --- Configuration ---

type FiveDimConfig struct {
	RingSize     int
	Lfast, Lslow float64 // Information-Time Lengths
	VarAlphaB    float64 // Decay for Imbalance Variance
	AlphaAbs     float64 // Decay for Absorption Baseline
	VarAlphaS    float64 // Decay for Surplus Variance
	AlphaE       float64 // Decay for Elasticity Reference
}

type VariantDef struct {
	ID  string
	Cfg FiveDimConfig
}

type ofiTask struct {
	Y, M, D        int
	Offset, Length int64
}

// --- Main Builder Entry ---

func runBuild() {
	start := time.Now()

	// Adaptive Base Config
	// Lfast=2.0 (approx 2 units of sqrt-vol), Lslow=300 (approx 5-10m regime)
	baseCfg := FiveDimConfig{
		RingSize:  20_000,
		Lfast:     2.0,
		Lslow:     300.0,
		VarAlphaB: 0.001,
		AlphaAbs:  0.01,
		VarAlphaS: 0.005,
		AlphaE:    0.001,
	}

	variants := []VariantDef{
		{ID: "5D_Adaptive_Base", Cfg: baseCfg},
		{
			ID: "5D_Adaptive_Fast",
			Cfg: func() FiveDimConfig {
				c := baseCfg
				c.Lfast = 0.5
				c.Lslow = 60.0
				return c
			}(),
		},
	}

	symbols := discoverSymbols()
	fmt.Printf("--- FEATURE BUILDER (M4 Optimized) | Found Symbols: %v ---\n", symbols)

	for _, sym := range symbols {
		buildForSymbol(sym, variants)
	}

	fmt.Printf("[build] ALL SYMBOLS COMPLETE in %s\n", time.Since(start))
}

func discoverSymbols() []string {
	var syms []string
	entries, err := os.ReadDir(BaseDir)
	if err != nil {
		return syms
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "features" || name == "common" || strings.HasPrefix(name, ".") {
			continue
		}
		syms = append(syms, name)
	}
	return syms
}

func buildForSymbol(sym string, variants []VariantDef) {
	fmt.Printf("\n>>> Building for %s <<<\n", sym)
	featRoot := filepath.Join(BaseDir, "features", sym)

	tasks := discoverTasks(sym)
	if len(tasks) == 0 {
		fmt.Printf("[warn] No data found for %s\n", sym)
		return
	}

	for _, v := range variants {
		buildVariant(sym, v, tasks, featRoot)
	}
}

func buildVariant(sym string, v VariantDef, tasks []ofiTask, featRoot string) {
	vStart := time.Now()
	outDir := filepath.Join(featRoot, v.ID)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Printf("[err] mkdir %s: %v\n", outDir, err)
		return
	}

	jobs := make(chan ofiTask, len(tasks))
	var wg sync.WaitGroup

	// Use dynamic worker count appropriate for the chip
	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Reusable buffer: 10M rows * 5 features * 8 bytes = 400MB
			binBuf := make([]byte, 0, BuildMaxRows*5*8)
			for t := range jobs {
				processBuildDay(sym, t, outDir, v.Cfg, &binBuf)
			}
		}()
	}

	for _, t := range tasks {
		jobs <- t
	}
	close(jobs)
	wg.Wait()

	fmt.Printf("[build] %s / %s done in %s\n", sym, v.ID, time.Since(vStart))
}

func processBuildDay(
	sym string,
	t ofiTask,
	outDir string,
	cfg FiveDimConfig,
	binBuf *[]byte,
) {
	dateStr := fmt.Sprintf("%04d%02d%02d", t.Y, t.M, t.D)
	outPath := filepath.Join(outDir, dateStr+".bin")

	// Skip if exists (incremental build)
	if _, err := os.Stat(outPath); err == nil {
		return
	}

	rawBytes, rowCount, ok := loadRawBlob(sym, t)
	if !ok || rowCount == 0 {
		return
	}

	n := int(rowCount)
	// Output Layout: [Row1_f1..f5] [Row2_f1..f5] ...
	// Size: n * 5 * 8 bytes
	reqSize := n * 5 * 8
	if cap(*binBuf) < reqSize {
		*binBuf = make([]byte, reqSize)
	}
	*binBuf = (*binBuf)[:reqSize]

	// Init 5D Engine
	core := NewCore(cfg.RingSize, cfg.Lfast, cfg.Lslow, cfg.VarAlphaB, cfg.AlphaAbs, cfg.VarAlphaS, cfg.AlphaE)
	engine := NewEngine(core)

	// Process Rows
	// ARM64 optimization: Unrolling loop slightly or keeping simple?
	// The branch predictor on M4 is excellent. Simple linear scan is best.
	for i := 0; i < n; i++ {
		off := i * RowSize
		row := ParseAggRow(rawBytes[off : off+RowSize])

		tr := Trade{
			Side:  TradeSign(row), // +1.0 or -1.0
			Qty:   TradeQty(row),
			Price: TradePrice(row),
			Ts:    row.TsMs,
		}

		feats := engine.Update(tr)

		// Write 5 features interleaved
		// LittleEndian is native for ARM64
		baseOff := i * 40
		binary.LittleEndian.PutUint64((*binBuf)[baseOff+0:], math.Float64bits(feats[0]))
		binary.LittleEndian.PutUint64((*binBuf)[baseOff+8:], math.Float64bits(feats[1]))
		binary.LittleEndian.PutUint64((*binBuf)[baseOff+16:], math.Float64bits(feats[2]))
		binary.LittleEndian.PutUint64((*binBuf)[baseOff+24:], math.Float64bits(feats[3]))
		binary.LittleEndian.PutUint64((*binBuf)[baseOff+32:], math.Float64bits(feats[4]))
	}

	if err := os.WriteFile(outPath, *binBuf, 0o644); err != nil {
		fmt.Printf("[err] write %s: %v\n", outPath, err)
	}
}

// --- 5D Adaptive Engine ---

// Core maintains the ring buffer.
// Using Structure of Arrays (SoA) matches ARM64 NEON optimization patterns.
type Core struct {
	N      int
	idx    int64
	filled bool

	// Ring Data (aligned arrays for cache locality)
	u     []float64
	side  []float64
	qty   []float64
	price []float64
	ts    []int64
	info  []float64 // cumulative info-time

	// Config
	Lfast, Lslow float64

	// State - EW Vars
	EWVarBf, EWVarBs, VarAlphaB float64
	// State - SFA
	AbsBaseline, AlphaAbs, EWVarS, VarAlphaS float64
	// State - Elasticity
	ERef, AlphaE float64
}

type Trade struct {
	Side  float64
	Qty   float64
	Price float64
	Ts    int64
}

type WindowSnap struct {
	Start, Count          int
	B                     float64
	PriceFirst, PriceLast float64
}

type SnapPair struct {
	Fast, Slow WindowSnap
}

func NewCore(N int, Lfast, Lslow, varAlphaB, alphaAbs, varAlphaS, alphaE float64) *Core {
	return &Core{
		N:         N,
		u:         make([]float64, N),
		side:      make([]float64, N),
		qty:       make([]float64, N),
		price:     make([]float64, N),
		ts:        make([]int64, N),
		info:      make([]float64, N),
		Lfast:     Lfast,
		Lslow:     Lslow,
		VarAlphaB: varAlphaB,
		AlphaAbs:  alphaAbs,
		VarAlphaS: varAlphaS,
		AlphaE:    alphaE,
	}
}

func (c *Core) Update(tr Trade) SnapPair {
	slot := int(c.idx % int64(c.N))

	// Info unit: u = signed sqrt(qty)
	// math.Sqrt is hardware accelerated on ARM64
	u := tr.Side * math.Sqrt(tr.Qty)

	var prevInfo float64
	if c.idx > 0 {
		prevInfo = c.info[(c.idx-1)%int64(c.N)]
	}
	info := prevInfo + math.Abs(u)

	c.u[slot] = u
	c.side[slot] = tr.Side
	c.qty[slot] = tr.Qty
	c.price[slot] = tr.Price
	c.ts[slot] = tr.Ts
	c.info[slot] = info

	c.idx++
	if c.idx >= int64(c.N) {
		c.filled = true
	}

	return c.buildSnaps()
}

// buildSnaps performs a backward scan on the ring buffer to find Fast/Slow windows
// based on information-time distance.
func (c *Core) buildSnaps() SnapPair {
	if c.idx == 0 {
		return SnapPair{}
	}
	lastPos := int((c.idx - 1) % int64(c.N))
	infoNow := c.info[lastPos]

	var Bf, Bs float64
	var countF, countS int
	startF, startS := lastPos, lastPos

	// Don't scan more than ring size or total history
	limit := c.N
	if int64(limit) > c.idx {
		limit = int(c.idx)
	}

	// Backward Scan
	// M4 has massive reorder buffers; loop unrolling isn't strictly necessary for scalar Go code.
	for k := 0; k < limit; k++ {
		i := (lastPos - k + c.N) % c.N

		// Distance in info-time from NOW to the END of trade i
		// infoNow is cumulative incl lastPos. info[i] is cumulative incl i.
		// We want the sum of |u| from (i+1) to lastPos.
		dist := infoNow - c.info[i]

		if dist > c.Lslow {
			break // Outside slow window
		}

		// Accumulate Slow
		Bs += c.u[i]
		countS++
		startS = i

		// Accumulate Fast
		if dist <= c.Lfast {
			Bf += c.u[i]
			countF++
			startF = i
		}
	}

	var fast, slow WindowSnap
	if countF > 0 {
		fast = WindowSnap{
			Start:      startF,
			Count:      countF,
			B:          Bf,
			PriceFirst: c.price[startF],
			PriceLast:  c.price[lastPos],
		}
	}
	if countS > 0 {
		slow = WindowSnap{
			Start:      startS,
			Count:      countS,
			B:          Bs,
			PriceFirst: c.price[startS],
			PriceLast:  c.price[lastPos],
		}
	}

	// Update Adaptive Variance Trackers
	if fast.Count > 0 {
		diff2 := fast.B * fast.B
		if c.EWVarBf == 0 {
			c.EWVarBf = diff2
		} else {
			c.EWVarBf = (1-c.VarAlphaB)*c.EWVarBf + c.VarAlphaB*diff2
		}
	}
	if slow.Count > 0 {
		diff2 := slow.B * slow.B
		if c.EWVarBs == 0 {
			c.EWVarBs = diff2
		} else {
			c.EWVarBs = (1-c.VarAlphaB)*c.EWVarBs + c.VarAlphaB*diff2
		}
	}

	return SnapPair{Fast: fast, Slow: slow}
}

// --- Feature Computation ---

type FeatureVector [5]float64

type Engine struct {
	Core *Core
}

func NewEngine(core *Core) *Engine {
	return &Engine{Core: core}
}

func (e *Engine) Update(tr Trade) FeatureVector {
	snaps := e.Core.Update(tr)
	fSnap := snaps.Fast
	sSnap := snaps.Slow

	if fSnap.Count == 0 || sSnap.Count == 0 {
		return FeatureVector{}
	}

	f1 := e.f1_Zfast(fSnap)
	f2 := e.f2_SFA(fSnap)
	f3 := e.f3_Elasticity(fSnap)
	f4 := e.f4_Coherence(fSnap)
	f5 := e.f5_Align(fSnap, sSnap, f4)

	return FeatureVector{f1, f2, f3, f4, f5}
}

// f1: Normalized Imbalance (TDFI Core)
func (e *Engine) f1_Zfast(f WindowSnap) float64 {
	scale := math.Sqrt(e.Core.EWVarBf + 1e-12)
	return math.Tanh(f.B / (scale + 1e-12))
}

// f2: Surplus Flow vs Absorption (SFA)
func (e *Engine) f2_SFA(f WindowSnap) float64 {
	c := e.Core
	B := f.B
	absB := math.Abs(B)

	// Update Absorption Baseline
	if c.AbsBaseline == 0 {
		c.AbsBaseline = absB
	} else {
		c.AbsBaseline = (1-c.AlphaAbs)*c.AbsBaseline + c.AlphaAbs*absB
	}

	surplus := absB - c.AbsBaseline
	if surplus <= 0 {
		return 0
	}
	S := math.Copysign(surplus, B)

	// Normalize Surplus
	diff2 := S * S
	if c.EWVarS == 0 {
		c.EWVarS = diff2
	} else {
		c.EWVarS = (1-c.VarAlphaS)*c.EWVarS + c.VarAlphaS*diff2
	}
	scale := math.Sqrt(c.EWVarS + 1e-12)
	return math.Tanh(S / (scale + 1e-12))
}

// f3: Flow Elasticity (Price Impact per Unit Flow)
func (e *Engine) f3_Elasticity(f WindowSnap) float64 {
	c := e.Core
	dP := f.PriceLast - f.PriceFirst
	Q := f.B
	eMag := math.Abs(dP) / (math.Abs(Q) + 1e-12)

	if c.ERef == 0 {
		c.ERef = eMag
	} else {
		c.ERef = (1-c.AlphaE)*c.ERef + c.AlphaE*eMag
	}

	r := eMag / (c.ERef + 1e-12)
	return math.Tanh(r)
}

// f4: Coherence (Entropy of Sign Process)
func (e *Engine) f4_Coherence(f WindowSnap) float64 {
	c := e.Core
	N := f.Count
	if N < 3 {
		return 0
	}

	start := f.Start
	var npp, npm, nmp, nmm float64
	var cntP, cntM float64

	prevIdx := start
	prevSide := c.side[prevIdx]

	// Forward scan in ring for transitions
	for k := 1; k < N; k++ {
		idx := (start + k) % c.N
		curSide := c.side[idx]

		if prevSide > 0 { // Buy
			cntP++
			if curSide > 0 {
				npp++
			} else {
				npm++
			}
		} else { // Sell
			cntM++
			if curSide > 0 {
				nmp++
			} else {
				nmm++
			}
		}
		prevIdx = idx
		prevSide = curSide
	}

	// Probabilities
	Ppp := npp / (npp + npm + 1e-12)
	Ppm := 1.0 - Ppp
	Pmp := nmp / (nmp + nmm + 1e-12)
	Pmm := 1.0 - Pmp

	piP := cntP / (float64(N-1) + 1e-12)
	piM := 1.0 - piP

	h := 0.0
	// Log2 is usually implemented via hardware instructions on ARM64
	if Ppp > 1e-9 {
		h -= piP * Ppp * math.Log2(Ppp)
	}
	if Ppm > 1e-9 {
		h -= piP * Ppm * math.Log2(Ppm)
	}
	if Pmp > 1e-9 {
		h -= piM * Pmp * math.Log2(Pmp)
	}
	if Pmm > 1e-9 {
		h -= piM * Pmm * math.Log2(Pmm)
	}

	C := 1.0 - h
	if C < 0 {
		C = 0
	}
	if C > 1 {
		C = 1
	}
	return C
}

// f5: Alignment (Fast + Slow * Coherence)
func (e *Engine) f5_Align(f, s WindowSnap, C float64) float64 {
	c := e.Core
	scaleF := math.Sqrt(c.EWVarBf + 1e-12)
	scaleS := math.Sqrt(c.EWVarBs + 1e-12)

	Zf := f.B / (scaleF + 1e-12)
	Zs := s.B / (scaleS + 1e-12)

	align := math.Tanh(Zf) * math.Tanh(Zs)
	return C * align
}

// --- IO Helpers ---

func discoverTasks(sym string) []ofiTask {
	root := filepath.Join(BaseDir, sym)
	var tasks []ofiTask
	years, err := os.ReadDir(root)
	if err != nil {
		return tasks
	}

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
			f, err := os.Open(idxPath)
			if err != nil {
				continue
			}
			var hdr [16]byte
			io.ReadFull(f, hdr[:])
			count := binary.LittleEndian.Uint64(hdr[8:])
			var row [26]byte

			for i := uint64(0); i < count; i++ {
				io.ReadFull(f, row[:])
				d := int(binary.LittleEndian.Uint16(row[0:]))
				offset := int64(binary.LittleEndian.Uint64(row[2:]))
				length := int64(binary.LittleEndian.Uint64(row[10:]))

				if d >= 1 && d <= 31 && length > 0 {
					tasks = append(tasks, ofiTask{
						Y: y, M: m, D: d,
						Offset: offset,
						Length: length,
					})
				}
			}
			f.Close()
		}
	}
	return tasks
}

func loadRawBlob(sym string, t ofiTask) ([]byte, uint64, bool) {
	dataPath := filepath.Join(BaseDir, sym, fmt.Sprintf("%04d", t.Y), fmt.Sprintf("%02d", t.M), "data.quantdev")

	f, err := os.Open(dataPath)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()

	if _, err := f.Seek(t.Offset, io.SeekStart); err != nil {
		return nil, 0, false
	}

	compData := make([]byte, t.Length)
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

	// Skip 48-byte AGG3 header
	if len(raw) < HeaderSize {
		return nil, 0, false
	}
	rowCount := binary.LittleEndian.Uint64(raw[8:])
	return raw[HeaderSize:], rowCount, true
}
