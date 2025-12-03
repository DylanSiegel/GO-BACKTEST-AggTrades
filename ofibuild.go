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

const (
	BuildMaxRows = 10_000_000
	eps          = 1e-12
)

// --- Builder Configuration ---

type FiveDimConfig struct {
	RingSize  int
	Lfast     float64 // information-time fast length
	Lslow     float64 // information-time slow length
	VarAlphaB float64 // EW decay for B^2
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
	baseCfg := FiveDimConfig{
		RingSize:  20_000,
		Lfast:     2.0,
		Lslow:     300.0,
		VarAlphaB: 0.001,
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
	fmt.Printf("--- FEATURE BUILDER (5D O(1) Engine) | Found Symbols: %v ---\n", symbols)

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

	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Reusable buffer; sized for max rows with current feature layout.
			binBuf := make([]byte, 0, BuildMaxRows*FeatRowBytes)
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

	if _, err := os.Stat(outPath); err == nil {
		return
	}

	rawBytes, rowCount, ok := loadRawBlob(sym, t)
	if !ok || rowCount == 0 {
		return
	}

	n := int(rowCount)
	reqSize := n * FeatRowBytes
	if cap(*binBuf) < reqSize {
		*binBuf = make([]byte, reqSize)
	}
	*binBuf = (*binBuf)[:reqSize]

	// Init O(1) Engine
	core := NewCore(cfg.RingSize, cfg.Lfast, cfg.Lslow, cfg.VarAlphaB)
	engine := NewEngine(core)

	// Process Rows
	for i := 0; i < n; i++ {
		off := i * RowSize
		row := ParseAggRow(rawBytes[off : off+RowSize])

		tr := Trade{
			Side:  TradeSign(row),
			Qty:   TradeQty(row),
			Price: TradePrice(row),
			Ts:    row.TsMs,
		}

		feats := engine.Update(tr)

		// Write 5 features interleaved as float32 on disk.
		baseOff := i * FeatRowBytes
		binary.LittleEndian.PutUint32((*binBuf)[baseOff+0:], math.Float32bits(float32(feats[0])))
		binary.LittleEndian.PutUint32((*binBuf)[baseOff+4:], math.Float32bits(float32(feats[1])))
		binary.LittleEndian.PutUint32((*binBuf)[baseOff+8:], math.Float32bits(float32(feats[2])))
		binary.LittleEndian.PutUint32((*binBuf)[baseOff+12:], math.Float32bits(float32(feats[3])))
		binary.LittleEndian.PutUint32((*binBuf)[baseOff+16:], math.Float32bits(float32(feats[4])))
	}

	if err := os.WriteFile(outPath, *binBuf, 0o644); err != nil {
		fmt.Printf("[err] write %s: %v\n", outPath, err)
	}
}

// --- 5D Adaptive Core (O(1) Info-Time Engine) ---

type Core struct {
	N    int
	head int64 // Monotonic trade counter

	// Ring Data (Access via head % N)
	u     []float64
	side  []float64
	price []float64
	info  []float64 // Absolute Cumulative info-time

	// O(1) Sliding Window State
	startF  int64   // Index of Fast window start
	startS  int64   // Index of Slow window start
	accumBf float64 // Running sum of u (Fast)
	accumBs float64 // Running sum of u (Slow)
	countF  int
	countS  int

	// Config
	Lfast, Lslow float64

	// EW variance of Bf and Bs
	EWVarBf, EWVarBs, VarAlphaB float64
}

type Trade struct {
	Side, Qty, Price float64
	Ts               int64
}

type WindowSnap struct {
	Start, Count          int
	B                     float64
	PriceFirst, PriceLast float64
}

type SnapPair struct {
	Fast, Slow WindowSnap
}

func NewCore(N int, Lfast, Lslow, varAlphaB float64) *Core {
	return &Core{
		N:         N,
		u:         make([]float64, N),
		side:      make([]float64, N),
		price:     make([]float64, N),
		info:      make([]float64, N),
		Lfast:     Lfast,
		Lslow:     Lslow,
		VarAlphaB: varAlphaB,
	}
}

// Update ingests a trade and returns the fast/slow window snapshots.
func (c *Core) Update(tr Trade) SnapPair {
	slot := int(c.head % int64(c.N))

	// 1. Information unit
	u := tr.Side * math.Sqrt(tr.Qty)

	var prevInfo float64
	if c.head > 0 {
		prevInfo = c.info[int((c.head-1)%int64(c.N))]
	}
	infoNow := prevInfo + math.Abs(u)

	// 2. Update ring
	c.u[slot] = u
	c.side[slot] = tr.Side
	c.price[slot] = tr.Price
	c.info[slot] = infoNow

	// 3. Add to accumulators
	c.accumBf += u
	c.accumBs += u
	c.countF++
	c.countS++

	// 4. Shrink fast window
	for c.startF < c.head {
		var infoPrev float64
		if c.startF > 0 {
			infoPrev = c.info[int((c.startF-1)%int64(c.N))]
		}
		if (infoNow - infoPrev) > c.Lfast {
			idxF := int(c.startF % int64(c.N))
			c.accumBf -= c.u[idxF]
			c.countF--
			c.startF++
		} else {
			break
		}
	}

	// Shrink slow window
	for c.startS < c.head {
		var infoPrev float64
		if c.startS > 0 {
			infoPrev = c.info[int((c.startS-1)%int64(c.N))]
		}
		if (infoNow - infoPrev) > c.Lslow {
			idxS := int(c.startS % int64(c.N))
			c.accumBs -= c.u[idxS]
			c.countS--
			c.startS++
		} else {
			break
		}
	}

	// 5. Construct snaps
	idxF := int(c.startF % int64(c.N))
	idxS := int(c.startS % int64(c.N))

	fast := WindowSnap{
		Start:      idxF,
		Count:      c.countF,
		B:          c.accumBf,
		PriceFirst: c.price[idxF],
		PriceLast:  tr.Price,
	}
	slow := WindowSnap{
		Start:      idxS,
		Count:      c.countS,
		B:          c.accumBs,
		PriceFirst: c.price[idxS],
		PriceLast:  tr.Price,
	}

	c.head++

	// 6. Update EW variance of Bf / Bs
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

// --- Kernel Feature Engine (new K1–K5 math) ---

type FeatureVector [FeatDims]float64

type Engine struct {
	Core *Core
	KS   KernelState
}

func NewEngine(core *Core) *Engine {
	return &Engine{
		Core: core,
		KS:   NewKernelState(DefaultKernelConfig()),
	}
}

func (e *Engine) Update(tr Trade) FeatureVector {
	snaps := e.Core.Update(tr)
	fSnap := snaps.Fast
	sSnap := snaps.Slow

	// If windows are too small, still update state but expect features ~0.
	if fSnap.Count == 0 || sSnap.Count == 0 {
		// We still compute coherence on the fast snapshot; this returns 0 if N<3.
		C := computeCoherence(e.Core, fSnap)
		return e.KS.Update(e.Core, fSnap, sSnap, C)
	}

	C := computeCoherence(e.Core, fSnap)
	return e.KS.Update(e.Core, fSnap, sSnap, C)
}

// --- Online stats abstractions ---

type EWStat struct {
	Alpha       float64
	Mean        float64
	Var         float64
	Initialized bool
}

func (s *EWStat) Update(x float64) {
	if s.Alpha <= 0 {
		return
	}
	if !s.Initialized {
		s.Mean = x
		s.Var = 0.0
		s.Initialized = true
		return
	}
	a := s.Alpha
	mOld := s.Mean
	mNew := (1-a)*mOld + a*x
	diff := x - mNew
	vNew := (1-a)*s.Var + a*diff*diff

	s.Mean = mNew
	s.Var = vNew
}

func (s *EWStat) Std() float64 {
	if !s.Initialized {
		return 0.0
	}
	return math.Sqrt(math.Max(s.Var, 0.0))
}

func (s *EWStat) ZScore(x float64) float64 {
	std := s.Std()
	if std <= 0 {
		return 0.0
	}
	return (x - s.Mean) / (std + eps)
}

// Robbins–Monro online quantile
type EWQuantile struct {
	Q           float64
	Step        float64
	Value       float64
	Initialized bool
}

func (q *EWQuantile) Update(x float64) {
	if q.Step <= 0 {
		return
	}
	if !q.Initialized {
		q.Value = x
		q.Initialized = true
		return
	}
	theta := q.Value
	if x <= theta {
		q.Value = theta - q.Step*(1.0-q.Q)
	} else {
		q.Value = theta + q.Step*q.Q
	}
}

func (q *EWQuantile) Current() float64 {
	if !q.Initialized {
		return 0.0
	}
	return q.Value
}

// --- Kernel configuration ---

type KernelConfig struct {
	// EW alphas for second-level stats
	AlphaVol      float64
	AlphaAct      float64
	AlphaC        float64
	AlphaElast    float64
	AlphaQuantile float64

	// Weight hyperparameters
	AlphaCoherence float64
	AlphaActTrend  float64
	AlphaActTail   float64
	AlphaFlow      float64
	AlphaAlign     float64
	AlphaSlow      float64
	AlphaR         float64
	AlphaElastW    float64
	AlphaFlatSlow  float64

	// Vol regime parameters
	VMid  float64
	Z0Mid float64
	ZHi0  float64
	VHi   float64

	// Scale / caps
	S1   float64
	S3   float64
	S4   float64
	S5   float64
	B2   float64
	B4   float64
	KMax float64
}

func DefaultKernelConfig() KernelConfig {
	return KernelConfig{
		AlphaVol:      0.01,
		AlphaAct:      0.01,
		AlphaC:        0.01,
		AlphaElast:    0.01,
		AlphaQuantile: 0.005,

		AlphaCoherence: 0.7,
		AlphaActTrend:  0.7,
		AlphaActTail:   1.0,
		AlphaFlow:      0.5,
		AlphaAlign:     0.3,
		AlphaSlow:      0.7,
		AlphaR:         0.7,
		AlphaElastW:    0.7,
		AlphaFlatSlow:  0.7,

		VMid:  1.5,
		Z0Mid: 0.75,
		ZHi0:  0.5,
		VHi:   1.5,

		S1:   3.0,
		S3:   3.0,
		S4:   3.0,
		S5:   2.0,
		B2:   1.5,
		B4:   1.0,
		KMax: 0.4,
	}
}

// --- Kernel state (per-symbol) ---

type KernelState struct {
	Cfg KernelConfig

	// Second-level EW stats
	VolStat   EWStat
	ActStat   EWStat
	CoherStat EWStat
	ElastStat EWStat

	ZfAbsQ80 EWQuantile
	ZfAbsQ98 EWQuantile

	// Internal vol of price impulse
	EWVarR float64

	// Latest normalized quantities
	Zf, Zs              float64
	ZVol, ZAct, CZ      float64
	RFastZ, ZElast      float64
	WCoherence          float64
	WVolMid, WVolHi     float64
	WActTrend, WActTail float64
	WNoise, WFlow       float64

	Initialized bool
}

func NewKernelState(cfg KernelConfig) KernelState {
	return KernelState{
		Cfg:       cfg,
		VolStat:   EWStat{Alpha: cfg.AlphaVol},
		ActStat:   EWStat{Alpha: cfg.AlphaAct},
		CoherStat: EWStat{Alpha: cfg.AlphaC},
		ElastStat: EWStat{Alpha: cfg.AlphaElast},
		ZfAbsQ80:  EWQuantile{Q: 0.80, Step: cfg.AlphaQuantile},
		ZfAbsQ98:  EWQuantile{Q: 0.98, Step: cfg.AlphaQuantile},
	}
}

// Update full state given current windows and coherence; return K1..K5.
func (ks *KernelState) Update(core *Core, f, s WindowSnap, coherence float64) FeatureVector {
	cfg := ks.Cfg

	// 1) Zf, Zs from EWVarBf/EWVarBs (assume zero-mean B).
	if core.EWVarBf > 0 {
		ks.Zf = f.B / (math.Sqrt(core.EWVarBf) + eps)
	} else {
		ks.Zf = 0
	}
	if core.EWVarBs > 0 {
		ks.Zs = s.B / (math.Sqrt(core.EWVarBs) + eps)
	} else {
		ks.Zs = 0
	}

	// 2) Fast price impulse and volatility.
	rFast := 0.0
	if f.PriceFirst > 0 {
		rFast = (f.PriceLast - f.PriceFirst) / (f.PriceFirst + eps)
	}

	r2 := rFast * rFast
	if r2 > 0 {
		if ks.EWVarR == 0 {
			ks.EWVarR = r2
		} else {
			a := cfg.AlphaVol
			ks.EWVarR = (1-a)*ks.EWVarR + a*r2
		}
	}
	sigma := math.Sqrt(ks.EWVarR + eps)

	if sigma > 0 {
		xVol := math.Log(sigma + eps)
		ks.VolStat.Update(xVol)
		ks.ZVol = ks.VolStat.ZScore(xVol)
	} else {
		ks.ZVol = 0
	}

	// 3) Activity proxy: number of trades in fast window.
	actRaw := float64(f.Count)
	if actRaw < 1 {
		actRaw = 1
	}
	xAct := math.Log(actRaw)
	ks.ActStat.Update(xAct)
	ks.ZAct = ks.ActStat.ZScore(xAct)

	// 4) Coherence normalization.
	ks.CoherStat.Update(coherence)
	ks.CZ = ks.CoherStat.ZScore(coherence)

	// 5) r_fast_z
	if sigma > 0 {
		ks.RFastZ = rFast / (sigma + eps)
	} else {
		ks.RFastZ = 0
	}

	// 6) Elasticity (price move per unit OFI).
	dP := f.PriceLast - f.PriceFirst
	eMag := math.Abs(dP) / (math.Abs(f.B) + eps)
	xEl := 0.0
	if eMag > 0 {
		xEl = math.Log(eMag + eps)
	}
	ks.ElastStat.Update(xEl)
	ks.ZElast = ks.ElastStat.ZScore(xEl)

	// 7) Quantiles of |Zf|.
	absZf := math.Abs(ks.Zf)
	ks.ZfAbsQ80.Update(absZf)
	ks.ZfAbsQ98.Update(absZf)

	// 8) Weights.

	// Coherence weight
	wCoherence := softstep(ks.CZ, cfg.AlphaCoherence)
	ks.WCoherence = clamp(wCoherence, 0.05, 0.99)

	// Mid-vol weight
	u := math.Max(math.Abs(ks.ZVol)-cfg.Z0Mid, 0.0)
	if cfg.VMid > 0 {
		ks.WVolMid = 1.0 / (1.0 + (u/cfg.VMid)*(u/cfg.VMid))
	} else {
		ks.WVolMid = 1.0
	}

	// High-vol weight
	v := math.Max(ks.ZVol-cfg.ZHi0, 0.0)
	if cfg.VHi > 0 {
		ks.WVolHi = 1.0 - 1.0/(1.0+(v/cfg.VHi)*(v/cfg.VHi))
	} else {
		ks.WVolHi = 0.0
	}

	// Activity weights
	baseTrend := softstep(ks.ZAct, cfg.AlphaActTrend)
	ks.WActTrend = clamp(0.3+0.7*baseTrend, 0.3, 1.0)
	ks.WActTail = softstep(ks.ZAct, cfg.AlphaActTail)

	// Noise and flow strength
	ks.WNoise = clamp(1.0-ks.WCoherence, 0.0, 0.95)
	ks.WFlow = math.Tanh(cfg.AlphaFlow * math.Abs(ks.Zf))

	ks.Initialized = true

	// 9) Kernels
	k1 := ks.computeK1()
	k2 := ks.computeK2()
	k3 := ks.computeK3()
	k4 := ks.computeK4()
	k5 := ks.computeK5()

	return FeatureVector{k1, k2, k3, k4, k5}
}

// K1 – Coherence-weighted fast OFI trend.
func (ks *KernelState) computeK1() float64 {
	cfg := ks.Cfg
	g1 := ks.Zf
	w1 := ks.WCoherence * ks.WActTrend * ks.WVolMid
	raw := w1 * g1
	return clamp(raw/(cfg.S1+eps), -1.0, 1.0)
}

// K2 – Tail OFI burst.
func (ks *KernelState) computeK2() float64 {
	cfg := ks.Cfg

	z0 := ks.ZfAbsQ80.Current()
	z1 := ks.ZfAbsQ98.Current()
	if z1 < z0+0.5 {
		z1 = z0 + 0.5
	}

	absZf := math.Abs(ks.Zf)
	excess := math.Max(absZf-z0, 0.0)
	tailFrac := clamp(excess/(z1-z0+eps), 0.0, 1.0)

	g2 := math.Copysign(tailFrac, ks.Zf)

	// Optional confirmation: only when price and flow agree.
	if ks.Zf*ks.RFastZ <= 0 {
		g2 = 0.0
	}

	w2 := ks.WCoherence * ks.WActTail
	raw := w2 * g2
	return math.Tanh(cfg.B2 * raw)
}

// K3 – Multi-scale alignment trend.
func (ks *KernelState) computeK3() float64 {
	cfg := ks.Cfg

	wSlowMag := math.Tanh(cfg.AlphaSlow * math.Abs(ks.Zs))
	corrLike := math.Tanh(cfg.AlphaAlign * ks.Zs * ks.Zf)
	wSlowDir := corrLike * wSlowMag
	alignment := (1.0 + wSlowDir) / 2.0 // [0,1]

	g3 := alignment * ks.Zf
	w3 := ks.WCoherence * ks.WVolMid

	raw := w3 * g3
	return clamp(raw/(cfg.S3+eps), -1.0, 1.0)
}

// K4 – Price/flow breakout continuation.
func (ks *KernelState) computeK4() float64 {
	cfg := ks.Cfg

	impMag := math.Tanh(cfg.AlphaR * math.Abs(ks.RFastZ))
	alignOK := ks.Zf*ks.RFastZ > 0

	var g4 float64
	if alignOK {
		g4Mag := math.Min(math.Abs(ks.Zf), math.Abs(ks.RFastZ))
		g4 = math.Copysign(g4Mag, ks.Zf)
	} else {
		g4 = 0.0
	}

	wImp := impMag * ks.WVolHi * ks.WActTail * ks.WCoherence
	raw := wImp * (g4 / (cfg.S4 + eps))
	return math.Tanh(cfg.B4 * raw)
}

// K5 – Overstretch mean-reversion (small, capped contrarian).
func (ks *KernelState) computeK5() float64 {
	cfg := ks.Cfg

	zE0 := 1.0
	excessE := math.Max(ks.ZElast-zE0, 0.0)
	wOver := math.Tanh(cfg.AlphaElastW * excessE)

	wFlat := 1.0 - math.Tanh(cfg.AlphaFlatSlow*math.Abs(ks.Zs))

	g5 := -ks.Zf
	w5 := wOver * wFlat * ks.WNoise * ks.WFlow

	raw := w5 * g5
	lin := raw / (cfg.S5 + eps)
	return clamp(lin, -cfg.KMax, cfg.KMax)
}

// --- Coherence (reused from old f4, but as a helper) ---

func computeCoherence(c *Core, f WindowSnap) float64 {
	N := f.Count
	if N < 3 {
		return 0
	}

	startIdx := int64(f.Start)

	var npp, npm, nmp, nmm float64
	var cntP, cntM float64

	prevIdx := int(startIdx % int64(c.N))
	prevSide := c.side[prevIdx]

	for k := 1; k < N; k++ {
		idx := int((startIdx + int64(k)) % int64(c.N))
		curSide := c.side[idx]

		if prevSide > 0 {
			cntP++
			if curSide > 0 {
				npp++
			} else {
				npm++
			}
		} else {
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

	Ppp := npp / (npp + npm + eps)
	Ppm := 1.0 - Ppp
	Pmp := nmp / (nmp + nmm + eps)
	Pmm := 1.0 - Pmp

	piP := cntP / (float64(N-1) + eps)
	piM := 1.0 - piP

	h := 0.0
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

// --- Small helpers ---

func clamp(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

func softstep(x, k float64) float64 {
	return 0.5 + 0.5*math.Tanh(k*x)
}

// --- IO Helpers (unchanged) ---

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
