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
	"sync"
	"time"
)

// --- Build Configuration ---

// Concurrency and buffer sizing for the feature builder.
const (
	BuildThreads = 24         // use all 24 logical cores
	BuildMaxRows = 10_000_000 // initial per-thread row budget for signal buffers
)

var BuildVariants = []VariantDef{
	{
		ID:    "A_Hawkes_Core",
		Model: "Hawkes2Scale",
		Cfg: Hawkes2ScaleConfig{
			TauFast: 5, TauSlow: 60,
			MuBuy: 0.1, MuSell: 0.1,
			A_pp_fast: 0.8, A_pm_fast: 0.2, A_mp_fast: 0.2, A_mm_fast: 0.8,
			A_pp_slow: 0.4, A_pm_slow: 0.1, A_mp_slow: 0.1, A_mm_slow: 0.4,
			D0: 50_000, VolLambda: 0.999, ZScoreLambda: 0.9995, SquashScale: 1.5,
		},
	},
	{
		ID:    "B_Hawkes_Adaptive",
		Model: "HawkesAdaptive",
		Cfg: HawkesAdaptiveConfig{
			HawkesCfg: Hawkes2ScaleConfig{
				TauFast: 2, TauSlow: 300,
				MuBuy: 0.1, MuSell: 0.1,
				A_pp_fast: 1.2, A_pm_fast: 0.0, A_mp_fast: 0.0, A_mm_fast: 1.2,
				A_pp_slow: 0.3, A_pm_slow: 0.1, A_mp_slow: 0.1, A_mm_slow: 0.3,
				D0: 50_000, VolLambda: 0.999, ZScoreLambda: 0.9999, SquashScale: 2.0,
			},
			ActivityLambda: 0.99, ActMid: 15.0, ActSlope: 2.0,
		},
	},
	{
		ID:    "C_MultiEMA_PowerLaw",
		Model: "MultiEMA",
		Cfg: MultiEMAConfig{
			TauSec:       []float64{10, 60, 300, 1800},
			Weights:      []float64{0.4, 0.3, 0.2, 0.1},
			D0:           50_000,
			VolLambda:    0.999,
			ZScoreLambda: 0.9995,
			SquashScale:  1.5,
		},
	},
	{
		ID:    "D_EMA_Baseline",
		Model: "EMA",
		Cfg: EMAConfig{
			TauSec:       30,
			D0:           50_000,
			VolLambda:    0.999,
			ZScoreLambda: 0.9995,
			SquashScale:  1.5,
		},
	},
}

// --- Main Builder ---

func runBuild() {
	start := time.Now()
	featRoot := filepath.Join(BaseDir, "features", Symbol)
	fmt.Printf("--- FEATURE BUILDER | %s | %d Variants ---\n", Symbol, len(BuildVariants))
	fmt.Printf("Output: %s\n", featRoot)

	// 1. Initialize Directories for each variant
	for _, v := range BuildVariants {
		_ = os.MkdirAll(filepath.Join(featRoot, v.ID), 0755)
	}

	// 2. Discover Tasks (only days that actually exist in index.quantdev)
	tasks := discoverTasks()
	fmt.Printf("[build] Processing %d days.\n", len(tasks))

	// Worker count (bounded by CPUThreads)
	workerCount := BuildThreads
	if workerCount > CPUThreads {
		workerCount = CPUThreads
	}
	fmt.Printf("[build] Using %d threads.\n", workerCount)

	// 3. Worker Pool
	var wg sync.WaitGroup
	jobs := make(chan ofiTask, len(tasks))

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Per-thread buffer reused to reduce GC.
			// Initial capacity is BuildMaxRows, but will grow if a day exceeds that.
			binBuf := make([]byte, 0, BuildMaxRows*8)

			for t := range jobs {
				processBuildDay(t, featRoot, &binBuf)
			}
		}()
	}

	for _, t := range tasks {
		jobs <- t
	}
	close(jobs)
	wg.Wait()

	fmt.Printf("[build] Complete in %s\n", time.Since(start))
}

// processBuildDay: load raw AGG3 data ONCE for (Y,M,D), then fan out to all variants.
func processBuildDay(t ofiTask, root string, binBuf *[]byte) {
	// 1. Load Data
	rawBytes, rowCount, ok := loadRawDay(t.Y, t.M, t.D)
	if !ok || rowCount == 0 {
		return
	}
	dateStr := fmt.Sprintf("%04d%02d%02d", t.Y, t.M, t.D)

	// 2. Process Each Variant
	for _, v := range BuildVariants {
		outPath := filepath.Join(root, v.ID, dateStr+".bin")

		// Skip if exists
		if _, err := os.Stat(outPath); err == nil {
			continue
		}

		// 3. Init Model State
		model := createModel(v)
		if model == nil {
			continue
		}

		// 4. Run Update Loop
		n := int(rowCount)
		reqSize := n * 8
		if cap(*binBuf) < reqSize {
			// In the rare case a day exceeds BuildMaxRows, grow the buffer once.
			*binBuf = make([]byte, reqSize)
		}
		*binBuf = (*binBuf)[:reqSize]

		for i := 0; i < n; i++ {
			off := i * RowSize
			row := ParseAggRow(rawBytes[off : off+RowSize])

			sig := model.Update(row)

			// Store as little-endian float64
			binary.LittleEndian.PutUint64((*binBuf)[i*8:], math.Float64bits(sig))
		}

		// 5. Write Disk
		if err := os.WriteFile(outPath, *binBuf, 0644); err != nil {
			fmt.Printf("[err] write %s: %v\n", outPath, err)
		}
	}
}

// --- Data Loader ---

func loadRawDay(y, m, d int) ([]byte, uint64, bool) {
	dir := filepath.Join(BaseDir, Symbol, fmt.Sprintf("%04d", y), fmt.Sprintf("%02d", m))
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

	hdr := make([]byte, 16)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return 0, 0
	}
	count := binary.LittleEndian.Uint64(hdr[8:])
	row := make([]byte, 26)
	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(f, row); err != nil {
			break
		}
		if int(binary.LittleEndian.Uint16(row[0:])) == day {
			return binary.LittleEndian.Uint64(row[2:]), binary.LittleEndian.Uint64(row[10:])
		}
	}
	return 0, 0
}

// discoverTasks builds a list of (Y,M,D) that actually exist in the index files.
// This avoids scheduling days that don't exist and reduces wasted I/O.
func discoverTasks() []ofiTask {
	root := filepath.Join(BaseDir, Symbol)
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
		if err != nil || y <= 0 {
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
			if err != nil || m < 1 || m > 12 {
				continue
			}

			idxPath := filepath.Join(root, yDir.Name(), mDir.Name(), "index.quantdev")
			f, err := os.Open(idxPath)
			if err != nil {
				continue
			}

			hdr := make([]byte, 16)
			if _, err := io.ReadFull(f, hdr); err != nil {
				f.Close()
				continue
			}
			// Optional: verify magic
			if string(hdr[:4]) != IdxMagic {
				f.Close()
				continue
			}

			count := binary.LittleEndian.Uint64(hdr[8:])
			row := make([]byte, 26)
			for i := uint64(0); i < count; i++ {
				if _, err := io.ReadFull(f, row); err != nil {
					break
				}
				d := int(binary.LittleEndian.Uint16(row[0:]))
				if d >= 1 && d <= 31 {
					tasks = append(tasks, ofiTask{Y: y, M: m, D: d})
				}
			}
			f.Close()
		}
	}
	return tasks
}

type ofiTask struct{ Y, M, D int }

// --- Model Logic & State Definitions ---

type VariantDef struct {
	ID    string
	Model string
	Cfg   interface{}
}

type SignalModel interface {
	Update(row AggRow) float64
}

func createModel(v VariantDef) SignalModel {
	switch v.Model {
	case "Hawkes2Scale":
		return NewHawkes2ScaleState(v.Cfg.(Hawkes2ScaleConfig))
	case "HawkesAdaptive":
		return NewHawkesAdaptiveState(v.Cfg.(HawkesAdaptiveConfig))
	case "MultiEMA":
		return NewMultiEMAState(v.Cfg.(MultiEMAConfig))
	case "EMA":
		return NewEMAState(v.Cfg.(EMAConfig))
	}
	return nil
}

// --- Shared Wrappers ---

type VolEWMA struct {
	Lambda  float64
	VarEwma float64
	LastPx  float64
	HasLast bool
}

func (v *VolEWMA) Update(price float64) {
	if !v.HasLast {
		v.LastPx = price
		v.HasLast = true
		return
	}
	if price <= 0 {
		return
	}
	r := math.Log(price / v.LastPx)
	v.LastPx = price
	v.VarEwma = v.Lambda*v.VarEwma + (1-v.Lambda)*r*r
}

func (v *VolEWMA) Sigma() float64 {
	if v.VarEwma <= 0 {
		return 0
	}
	return math.Sqrt(v.VarEwma)
}

type ZScoreEWMA struct {
	Lambda float64
	Mean   float64
	Var    float64
	Warmed bool
}

func (z *ZScoreEWMA) Update(x float64) float64 {
	if !z.Warmed {
		z.Mean = x
		z.Var = 0
		z.Warmed = true
		return 0
	}
	mPrev := z.Mean
	z.Mean = z.Lambda*z.Mean + (1-z.Lambda)*x
	dx := x - mPrev
	z.Var = z.Lambda*z.Var + (1-z.Lambda)*dx*dx
	if z.Var <= 0 {
		return 0
	}
	return (x - z.Mean) / math.Sqrt(z.Var)
}

func Squash(x, scale float64) float64 {
	return math.Tanh(scale * x)
}

// --- Model A: Hawkes 2-Scale ---

type Hawkes2ScaleConfig struct {
	TauFast, TauSlow, MuBuy, MuSell            float64
	A_pp_fast, A_pm_fast, A_mp_fast, A_mm_fast float64
	A_pp_slow, A_pm_slow, A_mp_slow, A_mm_slow float64
	D0, VolLambda, ZScoreLambda, SquashScale   float64
}

type Hawkes2ScaleState struct {
	cfg                   Hawkes2ScaleConfig
	lastTsMs              int64
	eBuyFast, eSellFast   float64
	eBuySlow, eSellSlow   float64
	lambdaBuy, lambdaSell float64
	vol                   VolEWMA
	z                     ZScoreEWMA
}

func NewHawkes2ScaleState(cfg Hawkes2ScaleConfig) *Hawkes2ScaleState {
	return &Hawkes2ScaleState{
		cfg: cfg,
		vol: VolEWMA{Lambda: cfg.VolLambda},
		z:   ZScoreEWMA{Lambda: cfg.ZScoreLambda},
	}
}

func (st *Hawkes2ScaleState) Update(row AggRow) float64 {
	ts := row.TsMs
	d := TradeDollar(row)
	s := TradeSign(row)
	px := TradePrice(row)

	// dt in seconds
	var dtSec float64
	if st.lastTsMs == 0 {
		st.lastTsMs = ts
		dtSec = 0
	} else {
		dtSec = float64(ts-st.lastTsMs) / 1000.0
		if dtSec < 0 {
			dtSec = 0
		}
		st.lastTsMs = ts
	}

	// decay excitations
	if dtSec > 0 {
		df := math.Exp((-1.0 / st.cfg.TauFast) * dtSec)
		ds := math.Exp((-1.0 / st.cfg.TauSlow) * dtSec)
		st.eBuyFast *= df
		st.eSellFast *= df
		st.eBuySlow *= ds
		st.eSellSlow *= ds
	}

	// log-saturated mark
	mark := 0.0
	if d > 0 && st.cfg.D0 > 0 {
		mark = math.Log(1.0 + d/st.cfg.D0)
	}

	// add excitation
	if s > 0 {
		st.eBuyFast += mark
		st.eBuySlow += mark
	} else {
		st.eSellFast += mark
		st.eSellSlow += mark
	}

	// intensities
	buy := st.cfg.MuBuy +
		(st.cfg.A_pp_fast*st.eBuyFast + st.cfg.A_pm_fast*st.eSellFast) +
		(st.cfg.A_pp_slow*st.eBuySlow + st.cfg.A_pm_slow*st.eSellSlow)

	sell := st.cfg.MuSell +
		(st.cfg.A_mp_fast*st.eBuyFast + st.cfg.A_mm_fast*st.eSellFast) +
		(st.cfg.A_mp_slow*st.eBuySlow + st.cfg.A_mm_slow*st.eSellSlow)

	if buy < 0 {
		buy = 0
	}
	if sell < 0 {
		sell = 0
	}

	st.lambdaBuy = buy
	st.lambdaSell = sell

	imb := 0.0
	if den := buy + sell; den > 1e-12 {
		imb = (buy - sell) / den
	}

	// vol & z
	st.vol.Update(px)
	sigma := st.vol.Sigma()
	if sigma <= 0 {
		sigma = 1
	}
	return Squash(st.z.Update(imb/sigma), st.cfg.SquashScale)
}

// --- Model B: Hawkes Adaptive ---

type HawkesAdaptiveConfig struct {
	HawkesCfg                        Hawkes2ScaleConfig
	ActivityLambda, ActMid, ActSlope float64
}

type HawkesAdaptiveState struct {
	base                                         Hawkes2ScaleConfig
	lastTsMs                                     int64
	eBuyFast, eSellFast                          float64
	eBuySlow, eSellSlow                          float64
	actLambda, actEWMA, actMid, actSlope, squash float64
	vol                                          VolEWMA
	z                                            ZScoreEWMA
}

func NewHawkesAdaptiveState(cfg HawkesAdaptiveConfig) *HawkesAdaptiveState {
	return &HawkesAdaptiveState{
		base:      cfg.HawkesCfg,
		actLambda: cfg.ActivityLambda,
		actMid:    cfg.ActMid,
		actSlope:  cfg.ActSlope,
		squash:    cfg.HawkesCfg.SquashScale,
		vol:       VolEWMA{Lambda: cfg.HawkesCfg.VolLambda},
		z:         ZScoreEWMA{Lambda: cfg.HawkesCfg.ZScoreLambda},
	}
}

func (st *HawkesAdaptiveState) Update(row AggRow) float64 {
	ts := row.TsMs
	d := TradeDollar(row)
	s := TradeSign(row)
	px := TradePrice(row)

	var dtSec float64
	if st.lastTsMs == 0 {
		st.lastTsMs = ts
		dtSec = 0
	} else {
		dtSec = float64(ts-st.lastTsMs) / 1000.0
		if dtSec < 0 {
			dtSec = 0
		}
		st.lastTsMs = ts
	}

	// activity EWMA and decay
	if dtSec > 0 {
		st.actEWMA = st.actLambda*st.actEWMA + (1-st.actLambda)*(1.0/dtSec)
		df := math.Exp((-1.0 / st.base.TauFast) * dtSec)
		ds := math.Exp((-1.0 / st.base.TauSlow) * dtSec)
		st.eBuyFast *= df
		st.eSellFast *= df
		st.eBuySlow *= ds
		st.eSellSlow *= ds
	}

	// mark
	mark := 0.0
	if d > 0 && st.base.D0 > 0 {
		mark = math.Log(1.0 + d/st.base.D0)
	}

	if s > 0 {
		st.eBuyFast += mark
		st.eBuySlow += mark
	} else {
		st.eSellFast += mark
		st.eSellSlow += mark
	}

	// fast kernel intensities
	bf := st.base.MuBuy + st.base.A_pp_fast*st.eBuyFast + st.base.A_pm_fast*st.eSellFast
	sf := st.base.MuSell + st.base.A_mp_fast*st.eBuyFast + st.base.A_mm_fast*st.eSellFast

	// slow kernel intensities
	bs := st.base.MuBuy + st.base.A_pp_slow*st.eBuySlow + st.base.A_pm_slow*st.eSellSlow
	ss := st.base.MuSell + st.base.A_mp_slow*st.eBuySlow + st.base.A_mm_slow*st.eSellSlow

	// activity-based slow weight (higher activity -> lower slow weight)
	wSlow := 0.5
	if st.actEWMA > 0 {
		x := (math.Log(st.actEWMA+1e-9) - math.Log(st.actMid+1e-9)) * st.actSlope
		wSlow = 1.0 / (1.0 + math.Exp(x))
	}
	if wSlow < 0 {
		wSlow = 0
	}
	if wSlow > 1 {
		wSlow = 1
	}
	wFast := 1.0 - wSlow

	buy := wFast*bf + wSlow*bs
	sell := wFast*sf + wSlow*ss
	if buy < 0 {
		buy = 0
	}
	if sell < 0 {
		sell = 0
	}

	imb := 0.0
	if den := buy + sell; den > 1e-12 {
		imb = (buy - sell) / den
	}

	st.vol.Update(px)
	sigma := st.vol.Sigma()
	if sigma <= 0 {
		sigma = 1
	}

	return Squash(st.z.Update(imb/sigma), st.squash)
}

// --- Model C: Multi-EMA Power-Law ---

type MultiEMAConfig struct {
	TauSec, Weights                          []float64
	D0, VolLambda, ZScoreLambda, SquashScale float64
}

type MultiEMAState struct {
	cfg      MultiEMAConfig
	lastTsMs int64
	ema      []float64
	vol      VolEWMA
	z        ZScoreEWMA
}

func NewMultiEMAState(cfg MultiEMAConfig) *MultiEMAState {
	return &MultiEMAState{
		cfg: cfg,
		ema: make([]float64, len(cfg.TauSec)),
		vol: VolEWMA{Lambda: cfg.VolLambda},
		z:   ZScoreEWMA{Lambda: cfg.ZScoreLambda},
	}
}

func (st *MultiEMAState) Update(row AggRow) float64 {
	ts := row.TsMs
	d := TradeDollar(row)
	s := TradeSign(row)
	px := TradePrice(row)

	var dtSec float64
	if st.lastTsMs == 0 {
		st.lastTsMs = ts
		dtSec = 0
	} else {
		dtSec = float64(ts-st.lastTsMs) / 1000.0
		if dtSec < 0 {
			dtSec = 0
		}
		st.lastTsMs = ts
	}

	mark := 0.0
	if d > 0 && st.cfg.D0 > 0 {
		mark = math.Log(1.0 + d/st.cfg.D0)
	}
	x := s * mark

	for j, tau := range st.cfg.TauSec {
		if tau <= 0 {
			continue
		}
		if dtSec > 0 {
			lambda := math.Exp(-dtSec / tau)
			st.ema[j] = lambda*st.ema[j] + (1-lambda)*x
		} else {
			st.ema[j] = x
		}
	}

	imb := 0.0
	for j, w := range st.cfg.Weights {
		imb += w * st.ema[j]
	}

	st.vol.Update(px)
	sigma := st.vol.Sigma()
	if sigma <= 0 {
		sigma = 1
	}

	return Squash(st.z.Update(imb/sigma), st.cfg.SquashScale)
}

// --- Model D: EMA Baseline ---

type EMAConfig struct {
	TauSec, D0, VolLambda, ZScoreLambda, SquashScale float64
}

type EMAState struct {
	cfg      EMAConfig
	lastTsMs int64
	ema      float64
	vol      VolEWMA
	z        ZScoreEWMA
}

func NewEMAState(cfg EMAConfig) *EMAState {
	return &EMAState{
		cfg: cfg,
		vol: VolEWMA{Lambda: cfg.VolLambda},
		z:   ZScoreEWMA{Lambda: cfg.ZScoreLambda},
	}
}

func (st *EMAState) Update(row AggRow) float64 {
	ts := row.TsMs
	d := TradeDollar(row)
	s := TradeSign(row)
	px := TradePrice(row)

	var dtSec float64
	if st.lastTsMs == 0 {
		st.lastTsMs = ts
		dtSec = 0
	} else {
		dtSec = float64(ts-st.lastTsMs) / 1000.0
		if dtSec < 0 {
			dtSec = 0
		}
		st.lastTsMs = ts
	}

	mark := 0.0
	if d > 0 && st.cfg.D0 > 0 {
		mark = math.Log(1.0 + d/st.cfg.D0)
	}
	x := s * mark

	if dtSec > 0 && st.cfg.TauSec > 0 {
		lambda := math.Exp(-dtSec / st.cfg.TauSec)
		st.ema = lambda*st.ema + (1-lambda)*x
	} else {
		st.ema = x
	}

	st.vol.Update(px)
	sigma := st.vol.Sigma()
	if sigma <= 0 {
		sigma = 1
	}

	return Squash(st.z.Update(st.ema/sigma), st.cfg.SquashScale)
}
