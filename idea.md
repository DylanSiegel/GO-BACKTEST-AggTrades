Make ofibuild.go and ofistudy.go make ofi study do a part oos samoplke too
so remove oos.go and sum.go can be deleted 

Good, let’s lock the design in. Below is a **compact “spec sheet” + Go-style pseudocode** for each model we kept:

* Common helpers (trade parsing, vol, z-score).
* Model A: Two-Scale Marked Hawkes TFI (Core).
* Model B: Activity-Adaptive Hawkes TFI.
* Model C: Constrained Multi-EMA Power-Law TFI.
* Model D: Inter-Trade-Time EMA Baseline.

All code is pure stdlib, stream-friendly, and designed to sit directly on top of your `agg` rows.

---

## 0. Shared primitives (used by all models)

### 0.1. Minimal trade view from your binary row

Assume you already have `[]byte row` with the layout you wrote earlier:

* `0:8` aggTradeId
* `8:16` price_fixed1e8
* `16:24` qty_fixed1e8
* `36:38` flags (is_buyer_maker in bit0)
* `38:46` ts_ms

You can wrap parsing:

```go
type AggRow struct {
    TsMs       int64
    PriceFixed uint64
    QtyFixed   uint64
    Flags      uint16
}

// cheap, no alloc; row is your 48/56-byte packed record
func ParseAggRow(row []byte) AggRow {
    return AggRow{
        TsMs:       int64(binary.LittleEndian.Uint64(row[38:])),
        PriceFixed: binary.LittleEndian.Uint64(row[8:]),
        QtyFixed:   binary.LittleEndian.Uint64(row[16:]),
        Flags:      binary.LittleEndian.Uint16(row[36:]),
    }
}

// taker sign from flags (Binance semantics)
func TradeSign(flags uint16) float64 {
    if flags&1 != 0 {
        // buyer is maker -> taker is seller
        return -1.0
    }
    return +1.0
}

// price, qty, dollar from fixed-point 1e8
func TradePrice(row AggRow) float64 {
    const scale = 1e-8
    return float64(row.PriceFixed) * scale
}

func TradeQty(row AggRow) float64 {
    const scale = 1e-8
    return float64(row.QtyFixed) * scale
}

func TradeDollar(row AggRow) float64 {
    p := TradePrice(row)
    q := TradeQty(row)
    return p * q
}
```

---

### 0.2. EWMA volatility on trade prices

We use trade price as mid proxy; you can replace later with better mid.

```go
type VolEWMA struct {
    Lambda   float64 // e.g. 0.97 for ~30s-ish memory with dense ticks
    VarEwma  float64
    LastPx   float64
    HasLast  bool
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
    // EWMA variance
    v.VarEwma = v.Lambda*v.VarEwma + (1-v.Lambda)*r*r
}

func (v *VolEWMA) Sigma() float64 {
    if v.VarEwma <= 0 {
        return 0
    }
    return math.Sqrt(v.VarEwma)
}
```

---

### 0.3. Z-score normalizer for any scalar signal

```go
type ZScoreEWMA struct {
    Lambda float64 // e.g. 0.99 for slow adaptation
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
    // EWMA mean
    mPrev := z.Mean
    z.Mean = z.Lambda*z.Mean + (1-z.Lambda)*x

    // EWMA variance of deviations
    dx := x - mPrev
    z.Var = z.Lambda*z.Var + (1-z.Lambda)*dx*dx

    if z.Var <= 0 {
        return 0
    }
    std := math.Sqrt(z.Var)
    return (x - z.Mean) / std
}
```

---

### 0.4. Utility: safe tanh-squash

```go
func Squash(x, scale float64) float64 {
    // scale ~ 1.0–3.0 depending on desired aggressiveness
    return math.Tanh(scale * x)
}
```

---

## A. Two-Scale Marked Hawkes TFI (Core)

### A.1. Concept

* Two exponential kernels (fast + slow) for buy and sell excitation.

* Marks: log-saturated dollar `m(d) = log(1 + d/d0)`.

* Intensities λ⁺, λ⁻ built from excitations.

* Core imbalance:

  [
  I(t) = \frac{\lambda^+ - \lambda^-}{\lambda^+ + \lambda^- + \varepsilon}
  ]

* Then vol-scale + z-score + tanh for final trading signal.

### A.2. State & parameters

```go
type Hawkes2ScaleConfig struct {
    // Time constants in seconds
    TauFast float64 // e.g. 5–10
    TauSlow float64 // e.g. 60–120

    // Baseline intensities (per second, rough scale)
    MuBuy  float64
    MuSell float64

    // Alpha matrix (fast kernel)
    A_pp_fast float64 // buy->buy
    A_pm_fast float64 // sell->buy
    A_mp_fast float64 // buy->sell
    A_mm_fast float64 // sell->sell

    // Alpha matrix (slow kernel)
    A_pp_slow float64
    A_pm_slow float64
    A_mp_slow float64
    A_mm_slow float64

    // Log-mark scale
    D0 float64 // typical dollar notional for mark~=1

    // Vol & z-score params
    VolLambda    float64
    ZScoreLambda float64

    // Output scaling
    SquashScale float64 // ~1–3
}

type Hawkes2ScaleState struct {
    cfg Hawkes2ScaleConfig

    lastTsMs int64

    // excitations (log-marked) per side & scale
    eBuyFast, eSellFast float64
    eBuySlow, eSellSlow float64

    // last intensities (for debugging / inspection)
    lambdaBuy  float64
    lambdaSell float64

    // risk wrappers
    vol VolEWMA
    z   ZScoreEWMA
}
```

### A.3. Initialization

```go
func NewHawkes2ScaleState(cfg Hawkes2ScaleConfig) *Hawkes2ScaleState {
    st := &Hawkes2ScaleState{cfg: cfg}
    st.vol = VolEWMA{Lambda: cfg.VolLambda}
    st.z = ZScoreEWMA{Lambda: cfg.ZScoreLambda}
    return st
}
```

### A.4. Per-trade update and signal

```go
func (st *Hawkes2ScaleState) Update(row AggRow) float64 {
    ts := row.TsMs
    d  := TradeDollar(row)
    s  := TradeSign(row.Flags)      // +1 or -1
    px := TradePrice(row)

    // 1) time delta (seconds)
    var dtSec float64
    if st.lastTsMs == 0 {
        st.lastTsMs = ts
        dtSec = 0
    } else {
        dtSec = float64(ts-st.lastTsMs) / 1000.0
        if dtSec < 0 {
            dtSec = 0 // defensive
        }
        st.lastTsMs = ts
    }

    // 2) decay excitations
    if dtSec > 0 {
        betaFast := 1.0 / st.cfg.TauFast
        betaSlow := 1.0 / st.cfg.TauSlow
        df := math.Exp(-betaFast * dtSec)
        ds := math.Exp(-betaSlow * dtSec)

        st.eBuyFast  *= df
        st.eSellFast *= df
        st.eBuySlow  *= ds
        st.eSellSlow *= ds
    }

    // 3) mark (log-saturated dollar)
    mark := 0.0
    if d > 0 && st.cfg.D0 > 0 {
        mark = math.Log(1.0 + d/st.cfg.D0)
    }

    // 4) add excitation
    if s > 0 {
        st.eBuyFast  += mark
        st.eBuySlow  += mark
    } else {
        st.eSellFast += mark
        st.eSellSlow += mark
    }

    // 5) intensities from excitations
    // fast
    buyFast := st.cfg.A_pp_fast*st.eBuyFast + st.cfg.A_pm_fast*st.eSellFast
    selFast := st.cfg.A_mp_fast*st.eBuyFast + st.cfg.A_mm_fast*st.eSellFast
    // slow
    buySlow := st.cfg.A_pp_slow*st.eBuySlow + st.cfg.A_pm_slow*st.eSellSlow
    selSlow := st.cfg.A_mp_slow*st.eBuySlow + st.cfg.A_mm_slow*st.eSellSlow

    st.lambdaBuy = st.cfg.MuBuy + buyFast + buySlow
    st.lambdaSell = st.cfg.MuSell + selFast + selSlow

    if st.lambdaBuy < 0 {
        st.lambdaBuy = 0
    }
    if st.lambdaSell < 0 {
        st.lambdaSell = 0
    }

    // 6) raw imbalance
    num := st.lambdaBuy - st.lambdaSell
    den := st.lambdaBuy + st.lambdaSell
    const eps = 1e-12
    var imb float64
    if den > eps {
        imb = num / (den + eps)
    } else {
        imb = 0
    }

    // 7) update vol
    st.vol.Update(px)
    sigma := st.vol.Sigma()
    if sigma <= 0 {
        sigma = 1 // avoid div by zero in early warm-up
    }

    // 8) vol-scaled imbalance
    imbVol := imb / sigma

    // 9) z-score normalization
    z := st.z.Update(imbVol)

    // 10) squashed trading signal
    sig := Squash(z, st.cfg.SquashScale)
    return sig
}
```

---

## B. Activity-Adaptive Hawkes TFI

### B.1. Concept

* Same Hawkes core, but separate fast/slow intensities and adaptive combination:

  * In high-activity bursts, rely more on fast component.
  * In quiet periods, rely more on slow component → longer memory.

### B.2. Extra state: activity

```go
type HawkesAdaptiveConfig struct {
    HawkesCfg Hawkes2ScaleConfig

    // Activity EWMA (trades/sec)
    ActivityLambda float64 // e.g. 0.99
    // Mapping activity -> slow weight parameters
    ActMid   float64 // "midpoint" trades/sec where weight_slow ~ 0.5
    ActSlope float64 // controls steepness of transition
}

type HawkesAdaptiveState struct {
    base Hawkes2ScaleConfig

    lastTsMs int64

    // excitations per kernel
    eBuyFast, eSellFast float64
    eBuySlow, eSellSlow float64

    // last intensities per kernel
    lambdaBuyFast, lambdaSellFast float64
    lambdaBuySlow, lambdaSellSlow float64

    // activity
    actLambda float64
    actEWMA   float64 // trades/sec estimate
    // vol & z
    vol VolEWMA
    z   ZScoreEWMA

    // mapping params
    actMid   float64
    actSlope float64
    squash   float64
}
```

### B.3. Initialization

```go
func NewHawkesAdaptiveState(cfg HawkesAdaptiveConfig) *HawkesAdaptiveState {
    st := &HawkesAdaptiveState{
        base:     cfg.HawkesCfg,
        actLambda: cfg.ActivityLambda,
        actMid:    cfg.ActMid,
        actSlope:  cfg.ActSlope,
        squash:    cfg.HawkesCfg.SquashScale,
    }
    st.vol = VolEWMA{Lambda: cfg.HawkesCfg.VolLambda}
    st.z   = ZScoreEWMA{Lambda: cfg.HawkesCfg.ZScoreLambda}
    return st
}
```

### B.4. Per-trade update and signal

```go
func (st *HawkesAdaptiveState) Update(row AggRow) float64 {
    ts := row.TsMs
    d  := TradeDollar(row)
    s  := TradeSign(row.Flags)
    px := TradePrice(row)

    // 1) dt and activity update
    var dtSec float64
    if st.lastTsMs == 0 {
        dtSec = 0
        st.lastTsMs = ts
    } else {
        dtSec = float64(ts-st.lastTsMs) / 1000.0
        if dtSec < 0 {
            dtSec = 0
        }
        st.lastTsMs = ts
    }

    // activity: trades/sec EWMA
    if dtSec > 0 {
        instRate := 1.0 / dtSec // crude instantaneous rate
        st.actEWMA = st.actLambda*st.actEWMA + (1-st.actLambda)*instRate
    }

    // 2) decay excitations
    if dtSec > 0 {
        betaFast := 1.0 / st.base.TauFast
        betaSlow := 1.0 / st.base.TauSlow
        df := math.Exp(-betaFast * dtSec)
        ds := math.Exp(-betaSlow * dtSec)

        st.eBuyFast  *= df
        st.eSellFast *= df
        st.eBuySlow  *= ds
        st.eSellSlow *= ds
    }

    // 3) log-mark
    mark := 0.0
    if d > 0 && st.base.D0 > 0 {
        mark = math.Log(1.0 + d/st.base.D0)
    }

    // 4) add excitation
    if s > 0 {
        st.eBuyFast  += mark
        st.eBuySlow  += mark
    } else {
        st.eSellFast += mark
        st.eSellSlow += mark
    }

    // 5) intensities per kernel
    // fast
    st.lambdaBuyFast = st.base.MuBuy +
        st.base.A_pp_fast*st.eBuyFast +
        st.base.A_pm_fast*st.eSellFast
    st.lambdaSellFast = st.base.MuSell +
        st.base.A_mp_fast*st.eBuyFast +
        st.base.A_mm_fast*st.eSellFast

    // slow
    st.lambdaBuySlow = st.base.MuBuy +
        st.base.A_pp_slow*st.eBuySlow +
        st.base.A_pm_slow*st.eSellSlow
    st.lambdaSellSlow = st.base.MuSell +
        st.base.A_mp_slow*st.eBuySlow +
        st.base.A_mm_slow*st.eSellSlow

    // 6) activity-based weight for slow kernel (sigmoid on log activity)
    // higher activity -> smaller slow weight
    act := st.actEWMA
    wSlow := 0.5
    if act > 0 {
        // logistic: wSlow ~ 1 at low act, ~0 at high act
        x := (math.Log(act+1e-9) - math.Log(st.actMid+1e-9)) * st.actSlope
        // 1 / (1+exp(x)), but flipped to be high at low x
        wSlow = 1.0 / (1.0 + math.Exp(x))
    }
    if wSlow < 0 {
        wSlow = 0
    } else if wSlow > 1 {
        wSlow = 1
    }
    wFast := 1.0 - wSlow

    // 7) blended intensities
    lambdaBuy := wFast*st.lambdaBuyFast + wSlow*st.lambdaBuySlow
    lambdaSell := wFast*st.lambdaSellFast + wSlow*st.lambdaSellSlow
    if lambdaBuy < 0 {
        lambdaBuy = 0
    }
    if lambdaSell < 0 {
        lambdaSell = 0
    }

    // 8) imbalance
    num := lambdaBuy - lambdaSell
    den := lambdaBuy + lambdaSell
    const eps = 1e-12
    imb := 0.0
    if den > eps {
        imb = num / (den + eps)
    }

    // 9) vol & z-score
    st.vol.Update(px)
    sigma := st.vol.Sigma()
    if sigma <= 0 {
        sigma = 1
    }
    imbVol := imb / sigma
    z := st.z.Update(imbVol)

    // 10) squash
    return Squash(z, st.squash)
}
```

---

## C. Constrained Multi-EMA Power-Law TFI

### C.1. Concept

* K EMAs of signed dollar flow with different τ_j.
* Weights `w_j` chosen once to approximate a power-law kernel.
* Same vol/z wrappers, then tanh.

### C.2. State and config

```go
type MultiEMAConfig struct {
    TauSec   []float64 // e.g. []float64{5, 20, 80, 320}
    Weights  []float64 // same length, fixed, sum to 1
    D0       float64   // for log mark or scaling
    VolLambda    float64
    ZScoreLambda float64
    SquashScale  float64
}

type MultiEMAState struct {
    cfg MultiEMAConfig

    lastTsMs int64
    ema      []float64 // per τ_j

    vol VolEWMA
    z   ZScoreEWMA
}
```

### C.3. Initialization

```go
func NewMultiEMAState(cfg MultiEMAConfig) *MultiEMAState {
    st := &MultiEMAState{cfg: cfg}
    st.ema = make([]float64, len(cfg.TauSec))
    st.vol = VolEWMA{Lambda: cfg.VolLambda}
    st.z   = ZScoreEWMA{Lambda: cfg.ZScoreLambda}
    return st
}
```

### C.4. Per-trade update and signal

```go
func (st *MultiEMAState) Update(row AggRow) float64 {
    ts := row.TsMs
    d  := TradeDollar(row)
    s  := TradeSign(row.Flags)
    px := TradePrice(row)

    // mark (choose one: linear, sublinear, or log)
    mark := 0.0
    if d > 0 && st.cfg.D0 > 0 {
        mark = math.Log(1.0 + d/st.cfg.D0)
    }

    // dt
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

    // update EMAs
    x := s * mark
    for j, tau := range st.cfg.TauSec {
        if tau <= 0 {
            continue
        }
        alpha := 1.0
        if dtSec > 0 {
            // continuous-time EMA decay
            lambda := math.Exp(-dtSec / tau)
            // so X_new = lambda*X_old + (1-lambda)*x
            st.ema[j] = lambda*st.ema[j] + (1-lambda)*x
        } else {
            // first tick, just set to x
            st.ema[j] = x
        }
        _ = alpha // just to show logic; not actually needed
    }

    // combine EMAs
    imb := 0.0
    for j, w := range st.cfg.Weights {
        imb += w * st.ema[j]
    }

    // vol & z
    st.vol.Update(px)
    sigma := st.vol.Sigma()
    if sigma <= 0 {
        sigma = 1
    }
    imbVol := imb / sigma
    z := st.z.Update(imbVol)

    return Squash(z, st.cfg.SquashScale)
}
```

---

## D. Inter-Trade-Time EMA Baseline

### D.1. Concept

* Single EMA on signed, marked dollar flow.
* Decay uses Δt; no Hawkes structure.
* Same vol/z wrappers.

### D.2. State & config

```go
type EMAConfig struct {
    TauSec       float64  // e.g. 20–40
    D0           float64  // for mark
    VolLambda    float64
    ZScoreLambda float64
    SquashScale  float64
}

type EMAState struct {
    cfg EMAConfig

    lastTsMs int64
    ema      float64

    vol VolEWMA
    z   ZScoreEWMA
}
```

### D.3. Initialization

```go
func NewEMAState(cfg EMAConfig) *EMAState {
    st := &EMAState{cfg: cfg}
    st.vol = VolEWMA{Lambda: cfg.VolLambda}
    st.z   = ZScoreEWMA{Lambda: cfg.ZScoreLambda}
    return st
}
```

### D.4. Per-trade update and signal

```go
func (st *EMAState) Update(row AggRow) float64 {
    ts := row.TsMs
    d  := TradeDollar(row)
    s  := TradeSign(row.Flags)
    px := TradePrice(row)

    // dt
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

    // mark
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

    // vol & z
    st.vol.Update(px)
    sigma := st.vol.Sigma()
    if sigma <= 0 {
        sigma = 1
    }
    imbVol := st.ema / sigma
    z := st.z.Update(imbVol)

    return Squash(z, st.cfg.SquashScale)
}
```

---

## How you’d wire this into your existing pipeline

At a high level, after you decompress one daily `agg` blob into `[]byte` rows:

```go
// example: using Hawkes2ScaleState + EMAState in parallel
func runSignalsOnDay(rows []byte, rowSize int, hawkes *Hawkes2ScaleState, ema *EMAState) {
    n := len(rows) / rowSize
    for i := 0; i < n; i++ {
        off := i * rowSize
        r := ParseAggRow(rows[off : off+rowSize])

        sigH := hawkes.Update(r)
        sigE := ema.Update(r)

        // here you can:
        // - snapshot signals every N ms,
        // - accumulate for backtest,
        // - write to file, etc.
        _ = sigH
        _ = sigE
    }
}
```

This gives you a clean, high-performance scaffold for:

* Hawkes-Core v1 (A),
* Hawkes-Adaptive (B),
* Multi-EMA (C),
* EMA baseline (D),

all running off the exact same `agg` binary you’re already generating.

Good, let’s lock the design in. Below is a **compact “spec sheet” + Go-style pseudocode** for each model we kept:

* Common helpers (trade parsing, vol, z-score).
* Model A: Two-Scale Marked Hawkes TFI (Core).
* Model B: Activity-Adaptive Hawkes TFI.
* Model C: Constrained Multi-EMA Power-Law TFI.
* Model D: Inter-Trade-Time EMA Baseline.

All code is pure stdlib, stream-friendly, and designed to sit directly on top of your `agg` rows.

---

## 0. Shared primitives (used by all models)

### 0.1. Minimal trade view from your binary row

Assume you already have `[]byte row` with the layout you wrote earlier:

* `0:8` aggTradeId
* `8:16` price_fixed1e8
* `16:24` qty_fixed1e8
* `36:38` flags (is_buyer_maker in bit0)
* `38:46` ts_ms

You can wrap parsing:

```go
type AggRow struct {
    TsMs       int64
    PriceFixed uint64
    QtyFixed   uint64
    Flags      uint16
}

// cheap, no alloc; row is your 48/56-byte packed record
func ParseAggRow(row []byte) AggRow {
    return AggRow{
        TsMs:       int64(binary.LittleEndian.Uint64(row[38:])),
        PriceFixed: binary.LittleEndian.Uint64(row[8:]),
        QtyFixed:   binary.LittleEndian.Uint64(row[16:]),
        Flags:      binary.LittleEndian.Uint16(row[36:]),
    }
}

// taker sign from flags (Binance semantics)
func TradeSign(flags uint16) float64 {
    if flags&1 != 0 {
        // buyer is maker -> taker is seller
        return -1.0
    }
    return +1.0
}

// price, qty, dollar from fixed-point 1e8
func TradePrice(row AggRow) float64 {
    const scale = 1e-8
    return float64(row.PriceFixed) * scale
}

func TradeQty(row AggRow) float64 {
    const scale = 1e-8
    return float64(row.QtyFixed) * scale
}

func TradeDollar(row AggRow) float64 {
    p := TradePrice(row)
    q := TradeQty(row)
    return p * q
}
```

---

### 0.2. EWMA volatility on trade prices

We use trade price as mid proxy; you can replace later with better mid.

```go
type VolEWMA struct {
    Lambda   float64 // e.g. 0.97 for ~30s-ish memory with dense ticks
    VarEwma  float64
    LastPx   float64
    HasLast  bool
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
    // EWMA variance
    v.VarEwma = v.Lambda*v.VarEwma + (1-v.Lambda)*r*r
}

func (v *VolEWMA) Sigma() float64 {
    if v.VarEwma <= 0 {
        return 0
    }
    return math.Sqrt(v.VarEwma)
}
```

---

### 0.3. Z-score normalizer for any scalar signal

```go
type ZScoreEWMA struct {
    Lambda float64 // e.g. 0.99 for slow adaptation
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
    // EWMA mean
    mPrev := z.Mean
    z.Mean = z.Lambda*z.Mean + (1-z.Lambda)*x

    // EWMA variance of deviations
    dx := x - mPrev
    z.Var = z.Lambda*z.Var + (1-z.Lambda)*dx*dx

    if z.Var <= 0 {
        return 0
    }
    std := math.Sqrt(z.Var)
    return (x - z.Mean) / std
}
```

---

### 0.4. Utility: safe tanh-squash

```go
func Squash(x, scale float64) float64 {
    // scale ~ 1.0–3.0 depending on desired aggressiveness
    return math.Tanh(scale * x)
}
```

---

## A. Two-Scale Marked Hawkes TFI (Core)

### A.1. Concept

* Two exponential kernels (fast + slow) for buy and sell excitation.

* Marks: log-saturated dollar `m(d) = log(1 + d/d0)`.

* Intensities λ⁺, λ⁻ built from excitations.

* Core imbalance:

  [
  I(t) = \frac{\lambda^+ - \lambda^-}{\lambda^+ + \lambda^- + \varepsilon}
  ]

* Then vol-scale + z-score + tanh for final trading signal.

### A.2. State & parameters

```go
type Hawkes2ScaleConfig struct {
    // Time constants in seconds
    TauFast float64 // e.g. 5–10
    TauSlow float64 // e.g. 60–120

    // Baseline intensities (per second, rough scale)
    MuBuy  float64
    MuSell float64

    // Alpha matrix (fast kernel)
    A_pp_fast float64 // buy->buy
    A_pm_fast float64 // sell->buy
    A_mp_fast float64 // buy->sell
    A_mm_fast float64 // sell->sell

    // Alpha matrix (slow kernel)
    A_pp_slow float64
    A_pm_slow float64
    A_mp_slow float64
    A_mm_slow float64

    // Log-mark scale
    D0 float64 // typical dollar notional for mark~=1

    // Vol & z-score params
    VolLambda    float64
    ZScoreLambda float64

    // Output scaling
    SquashScale float64 // ~1–3
}

type Hawkes2ScaleState struct {
    cfg Hawkes2ScaleConfig

    lastTsMs int64

    // excitations (log-marked) per side & scale
    eBuyFast, eSellFast float64
    eBuySlow, eSellSlow float64

    // last intensities (for debugging / inspection)
    lambdaBuy  float64
    lambdaSell float64

    // risk wrappers
    vol VolEWMA
    z   ZScoreEWMA
}
```

### A.3. Initialization

```go
func NewHawkes2ScaleState(cfg Hawkes2ScaleConfig) *Hawkes2ScaleState {
    st := &Hawkes2ScaleState{cfg: cfg}
    st.vol = VolEWMA{Lambda: cfg.VolLambda}
    st.z = ZScoreEWMA{Lambda: cfg.ZScoreLambda}
    return st
}
```

### A.4. Per-trade update and signal

```go
func (st *Hawkes2ScaleState) Update(row AggRow) float64 {
    ts := row.TsMs
    d  := TradeDollar(row)
    s  := TradeSign(row.Flags)      // +1 or -1
    px := TradePrice(row)

    // 1) time delta (seconds)
    var dtSec float64
    if st.lastTsMs == 0 {
        st.lastTsMs = ts
        dtSec = 0
    } else {
        dtSec = float64(ts-st.lastTsMs) / 1000.0
        if dtSec < 0 {
            dtSec = 0 // defensive
        }
        st.lastTsMs = ts
    }

    // 2) decay excitations
    if dtSec > 0 {
        betaFast := 1.0 / st.cfg.TauFast
        betaSlow := 1.0 / st.cfg.TauSlow
        df := math.Exp(-betaFast * dtSec)
        ds := math.Exp(-betaSlow * dtSec)

        st.eBuyFast  *= df
        st.eSellFast *= df
        st.eBuySlow  *= ds
        st.eSellSlow *= ds
    }

    // 3) mark (log-saturated dollar)
    mark := 0.0
    if d > 0 && st.cfg.D0 > 0 {
        mark = math.Log(1.0 + d/st.cfg.D0)
    }

    // 4) add excitation
    if s > 0 {
        st.eBuyFast  += mark
        st.eBuySlow  += mark
    } else {
        st.eSellFast += mark
        st.eSellSlow += mark
    }

    // 5) intensities from excitations
    // fast
    buyFast := st.cfg.A_pp_fast*st.eBuyFast + st.cfg.A_pm_fast*st.eSellFast
    selFast := st.cfg.A_mp_fast*st.eBuyFast + st.cfg.A_mm_fast*st.eSellFast
    // slow
    buySlow := st.cfg.A_pp_slow*st.eBuySlow + st.cfg.A_pm_slow*st.eSellSlow
    selSlow := st.cfg.A_mp_slow*st.eBuySlow + st.cfg.A_mm_slow*st.eSellSlow

    st.lambdaBuy = st.cfg.MuBuy + buyFast + buySlow
    st.lambdaSell = st.cfg.MuSell + selFast + selSlow

    if st.lambdaBuy < 0 {
        st.lambdaBuy = 0
    }
    if st.lambdaSell < 0 {
        st.lambdaSell = 0
    }

    // 6) raw imbalance
    num := st.lambdaBuy - st.lambdaSell
    den := st.lambdaBuy + st.lambdaSell
    const eps = 1e-12
    var imb float64
    if den > eps {
        imb = num / (den + eps)
    } else {
        imb = 0
    }

    // 7) update vol
    st.vol.Update(px)
    sigma := st.vol.Sigma()
    if sigma <= 0 {
        sigma = 1 // avoid div by zero in early warm-up
    }

    // 8) vol-scaled imbalance
    imbVol := imb / sigma

    // 9) z-score normalization
    z := st.z.Update(imbVol)

    // 10) squashed trading signal
    sig := Squash(z, st.cfg.SquashScale)
    return sig
}
```

---

## B. Activity-Adaptive Hawkes TFI

### B.1. Concept

* Same Hawkes core, but separate fast/slow intensities and adaptive combination:

  * In high-activity bursts, rely more on fast component.
  * In quiet periods, rely more on slow component → longer memory.

### B.2. Extra state: activity

```go
type HawkesAdaptiveConfig struct {
    HawkesCfg Hawkes2ScaleConfig

    // Activity EWMA (trades/sec)
    ActivityLambda float64 // e.g. 0.99
    // Mapping activity -> slow weight parameters
    ActMid   float64 // "midpoint" trades/sec where weight_slow ~ 0.5
    ActSlope float64 // controls steepness of transition
}

type HawkesAdaptiveState struct {
    base Hawkes2ScaleConfig

    lastTsMs int64

    // excitations per kernel
    eBuyFast, eSellFast float64
    eBuySlow, eSellSlow float64

    // last intensities per kernel
    lambdaBuyFast, lambdaSellFast float64
    lambdaBuySlow, lambdaSellSlow float64

    // activity
    actLambda float64
    actEWMA   float64 // trades/sec estimate
    // vol & z
    vol VolEWMA
    z   ZScoreEWMA

    // mapping params
    actMid   float64
    actSlope float64
    squash   float64
}
```

### B.3. Initialization

```go
func NewHawkesAdaptiveState(cfg HawkesAdaptiveConfig) *HawkesAdaptiveState {
    st := &HawkesAdaptiveState{
        base:     cfg.HawkesCfg,
        actLambda: cfg.ActivityLambda,
        actMid:    cfg.ActMid,
        actSlope:  cfg.ActSlope,
        squash:    cfg.HawkesCfg.SquashScale,
    }
    st.vol = VolEWMA{Lambda: cfg.HawkesCfg.VolLambda}
    st.z   = ZScoreEWMA{Lambda: cfg.HawkesCfg.ZScoreLambda}
    return st
}
```

### B.4. Per-trade update and signal

```go
func (st *HawkesAdaptiveState) Update(row AggRow) float64 {
    ts := row.TsMs
    d  := TradeDollar(row)
    s  := TradeSign(row.Flags)
    px := TradePrice(row)

    // 1) dt and activity update
    var dtSec float64
    if st.lastTsMs == 0 {
        dtSec = 0
        st.lastTsMs = ts
    } else {
        dtSec = float64(ts-st.lastTsMs) / 1000.0
        if dtSec < 0 {
            dtSec = 0
        }
        st.lastTsMs = ts
    }

    // activity: trades/sec EWMA
    if dtSec > 0 {
        instRate := 1.0 / dtSec // crude instantaneous rate
        st.actEWMA = st.actLambda*st.actEWMA + (1-st.actLambda)*instRate
    }

    // 2) decay excitations
    if dtSec > 0 {
        betaFast := 1.0 / st.base.TauFast
        betaSlow := 1.0 / st.base.TauSlow
        df := math.Exp(-betaFast * dtSec)
        ds := math.Exp(-betaSlow * dtSec)

        st.eBuyFast  *= df
        st.eSellFast *= df
        st.eBuySlow  *= ds
        st.eSellSlow *= ds
    }

    // 3) log-mark
    mark := 0.0
    if d > 0 && st.base.D0 > 0 {
        mark = math.Log(1.0 + d/st.base.D0)
    }

    // 4) add excitation
    if s > 0 {
        st.eBuyFast  += mark
        st.eBuySlow  += mark
    } else {
        st.eSellFast += mark
        st.eSellSlow += mark
    }

    // 5) intensities per kernel
    // fast
    st.lambdaBuyFast = st.base.MuBuy +
        st.base.A_pp_fast*st.eBuyFast +
        st.base.A_pm_fast*st.eSellFast
    st.lambdaSellFast = st.base.MuSell +
        st.base.A_mp_fast*st.eBuyFast +
        st.base.A_mm_fast*st.eSellFast

    // slow
    st.lambdaBuySlow = st.base.MuBuy +
        st.base.A_pp_slow*st.eBuySlow +
        st.base.A_pm_slow*st.eSellSlow
    st.lambdaSellSlow = st.base.MuSell +
        st.base.A_mp_slow*st.eBuySlow +
        st.base.A_mm_slow*st.eSellSlow

    // 6) activity-based weight for slow kernel (sigmoid on log activity)
    // higher activity -> smaller slow weight
    act := st.actEWMA
    wSlow := 0.5
    if act > 0 {
        // logistic: wSlow ~ 1 at low act, ~0 at high act
        x := (math.Log(act+1e-9) - math.Log(st.actMid+1e-9)) * st.actSlope
        // 1 / (1+exp(x)), but flipped to be high at low x
        wSlow = 1.0 / (1.0 + math.Exp(x))
    }
    if wSlow < 0 {
        wSlow = 0
    } else if wSlow > 1 {
        wSlow = 1
    }
    wFast := 1.0 - wSlow

    // 7) blended intensities
    lambdaBuy := wFast*st.lambdaBuyFast + wSlow*st.lambdaBuySlow
    lambdaSell := wFast*st.lambdaSellFast + wSlow*st.lambdaSellSlow
    if lambdaBuy < 0 {
        lambdaBuy = 0
    }
    if lambdaSell < 0 {
        lambdaSell = 0
    }

    // 8) imbalance
    num := lambdaBuy - lambdaSell
    den := lambdaBuy + lambdaSell
    const eps = 1e-12
    imb := 0.0
    if den > eps {
        imb = num / (den + eps)
    }

    // 9) vol & z-score
    st.vol.Update(px)
    sigma := st.vol.Sigma()
    if sigma <= 0 {
        sigma = 1
    }
    imbVol := imb / sigma
    z := st.z.Update(imbVol)

    // 10) squash
    return Squash(z, st.squash)
}
```

---

## C. Constrained Multi-EMA Power-Law TFI

### C.1. Concept

* K EMAs of signed dollar flow with different τ_j.
* Weights `w_j` chosen once to approximate a power-law kernel.
* Same vol/z wrappers, then tanh.

### C.2. State and config

```go
type MultiEMAConfig struct {
    TauSec   []float64 // e.g. []float64{5, 20, 80, 320}
    Weights  []float64 // same length, fixed, sum to 1
    D0       float64   // for log mark or scaling
    VolLambda    float64
    ZScoreLambda float64
    SquashScale  float64
}

type MultiEMAState struct {
    cfg MultiEMAConfig

    lastTsMs int64
    ema      []float64 // per τ_j

    vol VolEWMA
    z   ZScoreEWMA
}
```

### C.3. Initialization

```go
func NewMultiEMAState(cfg MultiEMAConfig) *MultiEMAState {
    st := &MultiEMAState{cfg: cfg}
    st.ema = make([]float64, len(cfg.TauSec))
    st.vol = VolEWMA{Lambda: cfg.VolLambda}
    st.z   = ZScoreEWMA{Lambda: cfg.ZScoreLambda}
    return st
}
```

### C.4. Per-trade update and signal

```go
func (st *MultiEMAState) Update(row AggRow) float64 {
    ts := row.TsMs
    d  := TradeDollar(row)
    s  := TradeSign(row.Flags)
    px := TradePrice(row)

    // mark (choose one: linear, sublinear, or log)
    mark := 0.0
    if d > 0 && st.cfg.D0 > 0 {
        mark = math.Log(1.0 + d/st.cfg.D0)
    }

    // dt
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

    // update EMAs
    x := s * mark
    for j, tau := range st.cfg.TauSec {
        if tau <= 0 {
            continue
        }
        alpha := 1.0
        if dtSec > 0 {
            // continuous-time EMA decay
            lambda := math.Exp(-dtSec / tau)
            // so X_new = lambda*X_old + (1-lambda)*x
            st.ema[j] = lambda*st.ema[j] + (1-lambda)*x
        } else {
            // first tick, just set to x
            st.ema[j] = x
        }
        _ = alpha // just to show logic; not actually needed
    }

    // combine EMAs
    imb := 0.0
    for j, w := range st.cfg.Weights {
        imb += w * st.ema[j]
    }

    // vol & z
    st.vol.Update(px)
    sigma := st.vol.Sigma()
    if sigma <= 0 {
        sigma = 1
    }
    imbVol := imb / sigma
    z := st.z.Update(imbVol)

    return Squash(z, st.cfg.SquashScale)
}
```

---

## D. Inter-Trade-Time EMA Baseline

### D.1. Concept

* Single EMA on signed, marked dollar flow.
* Decay uses Δt; no Hawkes structure.
* Same vol/z wrappers.

### D.2. State & config

```go
type EMAConfig struct {
    TauSec       float64  // e.g. 20–40
    D0           float64  // for mark
    VolLambda    float64
    ZScoreLambda float64
    SquashScale  float64
}

type EMAState struct {
    cfg EMAConfig

    lastTsMs int64
    ema      float64

    vol VolEWMA
    z   ZScoreEWMA
}
```

### D.3. Initialization

```go
func NewEMAState(cfg EMAConfig) *EMAState {
    st := &EMAState{cfg: cfg}
    st.vol = VolEWMA{Lambda: cfg.VolLambda}
    st.z   = ZScoreEWMA{Lambda: cfg.ZScoreLambda}
    return st
}
```

### D.4. Per-trade update and signal

```go
func (st *EMAState) Update(row AggRow) float64 {
    ts := row.TsMs
    d  := TradeDollar(row)
    s  := TradeSign(row.Flags)
    px := TradePrice(row)

    // dt
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

    // mark
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

    // vol & z
    st.vol.Update(px)
    sigma := st.vol.Sigma()
    if sigma <= 0 {
        sigma = 1
    }
    imbVol := st.ema / sigma
    z := st.z.Update(imbVol)

    return Squash(z, st.cfg.SquashScale)
}
```

---

## How you’d wire this into your existing pipeline

At a high level, after you decompress one daily `agg` blob into `[]byte` rows:

```go
// example: using Hawkes2ScaleState + EMAState in parallel
func runSignalsOnDay(rows []byte, rowSize int, hawkes *Hawkes2ScaleState, ema *EMAState) {
    n := len(rows) / rowSize
    for i := 0; i < n; i++ {
        off := i * rowSize
        r := ParseAggRow(rows[off : off+rowSize])

        sigH := hawkes.Update(r)
        sigE := ema.Update(r)

        // here you can:
        // - snapshot signals every N ms,
        // - accumulate for backtest,
        // - write to file, etc.
        _ = sigH
        _ = sigE
    }
}
```

This gives you a clean, high-performance scaffold for:

* Hawkes-Core v1 (A),
* Hawkes-Adaptive (B),
* Multi-EMA (C),
* EMA baseline (D),

all running off the exact same `agg` binary you’re already generating.