package main

import (
	"encoding/json"
	"math"
	"os"
	"sort"
)

// --- Master Data Structure ---

type AlphaResult struct {
	Date          string             `json:"date"`
	NBars         int                `json:"n_bars"`
	SignalQuality SignalQuality      `json:"signal_quality"`
	Horizons      map[string]Horizon `json:"horizons"` // "20", "50", "100"
}

type SignalQuality struct {
	Mean        float64 `json:"mean"`
	StdDev      float64 `json:"std_dev"`
	Skew        float64 `json:"skew"`
	Kurtosis    float64 `json:"kurtosis"`
	PctOutliers float64 `json:"pct_outliers"` // > 3 std
	Autocorr    float64 `json:"autocorr_lag1"`
	Turnover    float64 `json:"est_turnover_per_bar"` // |S_t - S_t-1|
}

type Horizon struct {
	ICPearson         float64 `json:"ic_pearson"`
	ICSpearman        float64 `json:"ic_spearman"`
	Beta              float64 `json:"beta"`
	TStat             float64 `json:"t_stat"`
	DecileSpreadBps   float64 `json:"decile_spread_bps"`
	BreakevenBps      float64 `json:"breakeven_cost_bps"` // The God Metric
	TheoreticalSharpe float64 `json:"theoretical_sharpe"`
}

// --- The Calculator ---

// CalculateAlphaMetrics runs the full suite for a specific horizon
func CalculateAlphaMetrics(sig, ret []float64) Horizon {
	h := Horizon{}
	n := len(sig)
	if n < 100 {
		return h
	}

	// 1. Predictive Power (IC)
	h.ICPearson = Correlation(sig, ret)
	h.ICSpearman = SpearmanCorrelation(sig, ret)

	// 2. Regression (Alpha/Beta)
	alpha, beta := SimpleOLS(sig, ret)
	h.Beta = beta

	// T-Stat calculation
	rss := 0.0
	sx := 0.0
	mx := Mean(sig)
	for i := 0; i < n; i++ {
		pred := alpha + beta*sig[i]
		resid := ret[i] - pred
		rss += resid * resid
		d := sig[i] - mx
		sx += d * d
	}
	stdErr := math.Sqrt(rss / float64(n-2))
	if sx > 0 {
		h.TStat = beta / (stdErr / math.Sqrt(sx))
	}

	// 3. Monotonicity (Decile Spread)
	h.DecileSpreadBps = CalcQuantileSpread(sig, ret, 10) * 10000

	// 4. Breakeven Cost (The most important HFT metric)
	// PnL = Sum(Sig * Ret)
	// Turnover = Sum(|Sig_t - Sig_t-1|)
	// BE = PnL / Turnover
	totalPnL := 0.0
	totalTurnover := 0.0
	for i := 0; i < n-1; i++ {
		totalPnL += sig[i] * ret[i]
		if i > 0 {
			totalTurnover += math.Abs(sig[i] - sig[i-1])
		}
	}
	if totalTurnover > 0 {
		h.BreakevenBps = (totalPnL / totalTurnover) * 10000
	}

	// 5. Theoretical Annualized Sharpe (Mark-to-Mid)
	// Assuming 5-min bars? Or tick bars?
	// We generalize: Sharpe = Mean(PnL) / Std(PnL) * Sqrt(N)
	pnl := make([]float64, n)
	for i := 0; i < n; i++ {
		pnl[i] = sig[i] * ret[i]
	}
	mPnl := Mean(pnl)
	sPnl := StdDev(pnl, mPnl) // Corrected: Uses shared.go StdDev(vals, mean)
	if sPnl > 0 {
		// Annualize assuming roughly 100k ticks/day for HFT or just per-bar
		// For comparison, we just output per-root-N
		h.TheoreticalSharpe = (mPnl / sPnl) * math.Sqrt(float64(n))
	}

	return h
}

// AnalyzeSignalQuality checks the "Health" of the alpha
func AnalyzeSignalQuality(sig []float64) SignalQuality {
	sq := SignalQuality{}
	n := len(sig)
	if n == 0 {
		return sq
	}

	// Moments
	sq.Mean = Mean(sig)
	sq.StdDev = StdDev(sig, sq.Mean) // Corrected: Uses shared.go StdDev(vals, mean)

	m3, m4 := 0.0, 0.0
	outliers := 0
	sumDiff := 0.0

	for i, v := range sig {
		d := 0.0
		if sq.StdDev > 0 {
			d = (v - sq.Mean) / sq.StdDev
		}
		m3 += d * d * d
		m4 += d * d * d * d
		if math.Abs(d) > 3.0 {
			outliers++
		}

		// Turnover proxy
		if i > 0 {
			sumDiff += math.Abs(v - sig[i-1])
		}
	}

	sq.Skew = m3 / float64(n)
	sq.Kurtosis = (m4 / float64(n)) - 3.0
	sq.PctOutliers = float64(outliers) / float64(n)
	sq.Turnover = sumDiff / float64(n)
	sq.Autocorr = AutoCorrelation(sig, 1)

	return sq
}

// --- Math Helpers (Optimized) ---

// NOTE: Mean, StdDev, and Correlation removed to resolve conflicts with shared.go

func AutoCorrelation(x []float64, lag int) float64 {
	n := len(x)
	if lag >= n {
		return 0
	}
	return Correlation(x[:n-lag], x[lag:])
}

func SimpleOLS(x, y []float64) (alpha, beta float64) {
	mx, my := Mean(x), Mean(y)
	num, den := 0.0, 0.0
	for i := 0; i < len(x); i++ {
		dx := x[i] - mx
		num += dx * (y[i] - my)
		den += dx * dx
	}
	if den == 0 {
		return 0, 0
	}
	beta = num / den
	alpha = my - beta*mx
	return
}

type rankPair struct {
	val float64
	idx int
}

func SpearmanCorrelation(x, y []float64) float64 {
	rx := getRanks(x)
	ry := getRanks(y)
	return Correlation(rx, ry)
}

func getRanks(v []float64) []float64 {
	n := len(v)
	pairs := make([]rankPair, n)
	for i, val := range v {
		pairs[i] = rankPair{val, i}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].val < pairs[j].val })
	ranks := make([]float64, n)
	for i, p := range pairs {
		ranks[p.idx] = float64(i + 1)
	}
	return ranks
}

func CalcQuantileSpread(sig, ret []float64, buckets int) float64 {
	n := len(sig)
	pairs := make([]rankPair, n)
	for i, val := range sig {
		pairs[i] = rankPair{val, i}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].val < pairs[j].val })
	sz := n / buckets
	if sz == 0 {
		return 0
	}
	sumBot, sumTop := 0.0, 0.0
	for i := 0; i < sz; i++ {
		sumBot += ret[pairs[i].idx]
	}
	for i := n - sz; i < n; i++ {
		sumTop += ret[pairs[i].idx]
	}
	return (sumTop / float64(sz)) - (sumBot / float64(sz))
}

func SaveReport(report []AlphaResult, path string) {
	f, _ := os.Create(path)
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.Encode(report)
}
