package main

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"sort"
)

// --- Top-Level Types --------------------------------------------------------

// AlphaMetrics represents the full metrics set for one signal on one dataset
// (e.g., one day, or one long run across many days).
// Horizons map contains per-horizon performance stats keyed by horizon string
// (e.g. "20", "50", "100").
type AlphaMetrics struct {
	Label   string                    `json:"label"`  // e.g. "BTCUSDT|OFI_VolumeTime_50M|2024-01-01"
	NBars   int                       `json:"n_bars"` // number of samples used
	Signal  SignalMetrics             `json:"signal"` // distribution / quality of the raw signal
	Horizon map[string]HorizonMetrics `json:"horizons"`
}

// SignalMetrics describes properties of the raw alpha signal (no horizon).
type SignalMetrics struct {
	Mean        float64 `json:"mean"`
	StdDev      float64 `json:"std_dev"`
	Skew        float64 `json:"skew"`
	Kurtosis    float64 `json:"kurtosis"`
	PctOutliers float64 `json:"pct_outliers"`
	Autocorr    float64 `json:"autocorr_lag1"`
	Turnover    float64 `json:"turnover_per_bar"`
	ZeroPct     float64 `json:"pct_zero_signal"`
	Hurst       float64 `json:"hurst_exponent"`
	Entropy     float64 `json:"shannon_entropy"`
}

// HorizonMetrics describes predictive and PnL properties for a given
// future-horizon (e.g. +20, +50, +100 ticks).
type HorizonMetrics struct {
	// Predictive Power
	ICPearson         float64 `json:"ic_pearson"`          // Pearson IC
	ICSpearman        float64 `json:"ic_spearman"`         // Rank IC
	ICStd             float64 `json:"ic_std"`              // Std dev of rolling IC
	ICIR              float64 `json:"ic_ir"`               // Information ratio of IC (mean/std of rolling IC)
	ShuffledICPearson float64 `json:"shuffled_ic_pearson"` // IC after shuffling returns (leakage sanity)

	TStat          float64 `json:"t_stat_beta"`     // t-stat of regression beta
	DirectionalHit float64 `json:"directional_hit"` // P(sign(sig) == sign(ret))

	// Linear Relationship
	Alpha float64 `json:"alpha"` // regression intercept (returns space)
	Beta  float64 `json:"beta"`  // regression slope (signal -> returns)

	// Monotonicity / Execution
	DecileSpreadBps float64 `json:"decile_spread_bps"` // top-bottom decile spread
	BreakevenBps    float64 `json:"breakeven_cost_bps"`
	FillRate        float64 `json:"fill_rate_est"`

	// Returns Profile (Sharpe-style)
	TheoreticalSharpe float64 `json:"theoretical_sharpe"`
	HACSharpe         float64 `json:"hac_sharpe"`
	ProbSharpeRatio   float64 `json:"prob_sharpe_ratio"`
	SortinoRatio      float64 `json:"sortino_ratio"`
	CalmarRatio       float64 `json:"calmar_ratio"`

	// Trade Statistics
	WinRate      float64 `json:"win_rate"`
	ProfitFactor float64 `json:"profit_factor"`
	AvgWinLoss   float64 `json:"avg_win_loss_ratio"`

	// Alpha Decay
	AlphaHalfLifeBars float64 `json:"alpha_half_life_bars"` // half-life in bars
	AlphaHalfLifeMs   float64 `json:"alpha_half_life_ms"`   // half-life in milliseconds (filled by caller)
}

// --- Signal / Horizon Metric Calculators -----------------------------------

// ComputeSignalMetrics analyzes the distribution and stability of a raw signal
// (no horizon notion here).
func ComputeSignalMetrics(sig []float64) SignalMetrics {
	s := SignalMetrics{}
	n := len(sig)
	if n == 0 {
		return s
	}

	m := Mean(sig)
	s.Mean = m
	s.StdDev = StdDev(sig, m)

	if s.StdDev == 0 {
		// Degenerate constant signal
		return s
	}

	outliers := 0
	zeros := 0
	sumDiff := 0.0

	sum3 := 0.0
	sum4 := 0.0

	for i, v := range sig {
		if math.Abs((v-m)/s.StdDev) > 3.0 {
			outliers++
		}
		if math.Abs(v) < 1e-9 {
			zeros++
		}
		if i > 0 {
			sumDiff += math.Abs(v - sig[i-1])
		}
		d := (v - m) / s.StdDev
		sum3 += d * d * d
		sum4 += d * d * d * d
	}

	s.PctOutliers = float64(outliers) / float64(n)
	s.Turnover = sumDiff / float64(n)
	s.ZeroPct = float64(zeros) / float64(n)
	s.Skew = sum3 / float64(n)
	s.Kurtosis = (sum4 / float64(n)) - 3.0

	s.Autocorr = AutoCorrelation(sig, 1)
	s.Hurst = EstimateHurst(sig)
	s.Entropy = EstimateEntropy(sig, 50)

	return s
}

// ComputeHorizonMetrics computes all key directional + PnL metrics for a given
// signal and aligned future returns at some horizon.
func ComputeHorizonMetrics(sig, ret []float64) HorizonMetrics {
	h := HorizonMetrics{}
	n := len(sig)
	if n == 0 || n != len(ret) {
		return h
	}
	if n < 200 {
		// Require some minimum sample size for stable stats
		return h
	}

	// 1. Basic correlations / regression
	h.ICPearson = Correlation(sig, ret)
	h.ICSpearman = SpearmanCorrelation(sig, ret)

	alpha, beta := SimpleOLS(sig, ret)
	h.Alpha = alpha
	h.Beta = beta

	// T-Stat on beta
	mx := Mean(sig)
	rss, sx := 0.0, 0.0
	for i := 0; i < n; i++ {
		pred := alpha + beta*sig[i]
		resid := ret[i] - pred
		rss += resid * resid
		d := sig[i] - mx
		sx += d * d
	}
	if sx > 0 {
		stdErr := math.Sqrt(rss / float64(n-2))
		if stdErr > 0 {
			h.TStat = beta / (stdErr / math.Sqrt(sx))
		}
	}

	// 2. Directional hit rate
	h.DirectionalHit = directionalHitRate(sig, ret)

	// 3. Monotonicity via quantile spread
	h.DecileSpreadBps = CalcQuantileSpread(sig, ret, 10) * 10000.0

	// 4. PnL stream & execution-style metrics
	var (
		grossWin, grossLoss   float64
		wins, losses          float64
		totalPnL, turnover    float64
		downsideSq, returnsSq float64
		peakCumPnl, maxDD     float64
		cumPnl                float64
		pnlStream             = make([]float64, n)
		filledCount           = 0.0
	)

	prevSig := 0.0
	for i := 0; i < n; i++ {
		// Fill heuristic: we assume a trade is filled if |ret| > 0.5 bps
		if math.Abs(ret[i]) > 0.00005 {
			filledCount++
		}

		pnl := sig[i] * ret[i]
		pnlStream[i] = pnl
		totalPnL += pnl

		if i > 0 {
			turnover += math.Abs(sig[i] - prevSig)
		}
		prevSig = sig[i]

		if pnl > 0 {
			grossWin += pnl
			wins++
		} else if pnl < 0 {
			grossLoss += math.Abs(pnl)
			losses++
			downsideSq += pnl * pnl
		}

		cumPnl += pnl
		if cumPnl > peakCumPnl {
			peakCumPnl = cumPnl
		}
		if (peakCumPnl - cumPnl) > maxDD {
			maxDD = peakCumPnl - cumPnl
		}
		returnsSq += pnl * pnl
	}

	// Fill rate
	h.FillRate = filledCount / float64(n)

	// Breakeven cost (bps) = PnL per unit turnover
	if turnover > 0 {
		h.BreakevenBps = (totalPnL / turnover) * 10000.0
	}

	// Hit stats
	if (wins + losses) > 0 {
		h.WinRate = wins / (wins + losses)
	}
	if grossLoss > 0 {
		h.ProfitFactor = grossWin / grossLoss
		if wins > 0 {
			avgWin := grossWin / wins
			avgLoss := grossLoss / losses
			if avgLoss > 0 {
				h.AvgWinLoss = avgWin / avgLoss
			}
		}
	} else if grossWin > 0 {
		h.ProfitFactor = 100.0
	}

	// 5. Sharpe / Sortino / Calmar
	meanPnl := totalPnL / float64(n)
	varPnl := (returnsSq / float64(n)) - (meanPnl * meanPnl)
	stdPnl := math.Sqrt(varPnl)

	// barsPerYear: adjust if you change horizon semantics
	const barsPerYear = 288.0 * 365.0 // 5-min-equivalent

	if stdPnl > 0 {
		h.TheoreticalSharpe = (meanPnl / stdPnl) * math.Sqrt(barsPerYear)

		if downsideSq > 0 {
			downsideDev := math.Sqrt(downsideSq / float64(n))
			if downsideDev > 0 {
				h.SortinoRatio = (meanPnl / downsideDev) * math.Sqrt(barsPerYear)
			}
		}
	}

	if maxDD > 0 {
		h.CalmarRatio = totalPnL / maxDD
	}

	// 6. HAC Sharpe & Prob(SR>0)
	rho := AutoCorrelation(pnlStream, 1)
	nwAdj := 1.0
	if math.Abs(rho) < 1.0 {
		nwAdj = math.Sqrt(1.0 - rho*rho)
	}
	h.HACSharpe = h.TheoreticalSharpe * nwAdj

	if stdPnl > 0 {
		skew, kurt := CalcHigherMoments(pnlStream)
		sr := meanPnl / stdPnl
		denominator := math.Sqrt(1.0 - skew*sr + ((kurt-1.0)/4.0)*sr*sr)
		if denominator > 0 {
			zVal := (sr * math.Sqrt(float64(n)-1.0)) / denominator
			h.ProbSharpeRatio = NormalCDF(zVal)
		}
	}

	// 7. Alpha half-life estimation from autocorrelation of signal
	h.AlphaHalfLifeBars = estimateHalfLifeBars(sig)

	// 8. Rolling IC stats for ICIR
	const icWindow = 2000
	meanICw, stdICw := rollingICStats(sig, ret, icWindow)
	if stdICw > 0 {
		h.ICStd = stdICw
		h.ICIR = meanICw / stdICw
	}

	// 9. Permutation IC sanity (anti-leakage check)
	h.ShuffledICPearson = shuffledIC(sig, ret)

	return h
}

// directionalHitRate: probability that signal and return have matching sign.
func directionalHitRate(sig, ret []float64) float64 {
	n := len(sig)
	if n == 0 || n != len(ret) {
		return 0
	}
	hits := 0
	valid := 0
	for i := 0; i < n; i++ {
		s := sig[i]
		r := ret[i]
		if s == 0 || r == 0 {
			continue
		}
		valid++
		if (s > 0 && r > 0) || (s < 0 && r < 0) {
			hits++
		}
	}
	if valid == 0 {
		return 0
	}
	return float64(hits) / float64(valid)
}

// estimateHalfLifeBars computes a half-life in "bars" based on log-ACF vs lag.
func estimateHalfLifeBars(sig []float64) float64 {
	n := len(sig)
	if n < 3 {
		return 0
	}
	lags := []float64{1, 2, 3, 4, 5}
	logAc := make([]float64, len(lags))
	for k := range lags {
		ac := AutoCorrelation(sig, int(lags[k]))
		if ac <= 0 {
			ac = 0.0001
		}
		logAc[k] = math.Log(ac)
	}
	_, slope := SimpleOLS(lags, logAc)
	if slope < 0 {
		// half-life in "bars"
		return -0.693147 / slope
	}
	return 0
}

// rollingICStats computes mean/std of IC over non-overlapping windows.
func rollingICStats(sig, ret []float64, window int) (meanIC, stdIC float64) {
	n := len(sig)
	if n != len(ret) || n < window*2 {
		return 0, 0
	}
	// non-overlapping windows
	var ics []float64
	for i := 0; i+window <= n; i += window {
		ic := Correlation(sig[i:i+window], ret[i:i+window])
		ics = append(ics, ic)
	}
	if len(ics) < 2 {
		return 0, 0
	}
	m := Mean(ics)
	sumSq := 0.0
	for _, v := range ics {
		d := v - m
		sumSq += d * d
	}
	std := math.Sqrt(sumSq / float64(len(ics)-1))
	return m, std
}

// shuffledIC computes IC between signal and a shuffled version of ret.
func shuffledIC(sig, ret []float64) float64 {
	n := len(sig)
	if n == 0 || n != len(ret) {
		return 0
	}
	tmp := make([]float64, n)
	copy(tmp, ret)
	// deterministic seed based on length
	r := rand.New(rand.NewSource(int64(n)*7919 + 1234567))
	for i := n - 1; i > 0; i-- {
		j := r.Intn(i + 1)
		tmp[i], tmp[j] = tmp[j], tmp[i]
	}
	return Correlation(sig, tmp)
}

// --- Advanced Math Helpers --------------------------------------------------

// CalcHigherMoments returns (skew, excess kurtosis) of x.
func CalcHigherMoments(x []float64) (skew, kurt float64) {
	n := float64(len(x))
	if n < 3 {
		return 0, 0
	}
	m := Mean(x)
	s := StdDev(x, m)
	if s == 0 {
		return 0, 0
	}

	sum3, sum4 := 0.0, 0.0
	for _, v := range x {
		d := (v - m) / s
		sum3 += d * d * d
		sum4 += d * d * d * d
	}
	skew = sum3 / n
	kurt = (sum4 / n) - 3.0
	return
}

// NormalCDF is Phi(x) for standard normal.
func NormalCDF(x float64) float64 {
	return 0.5 * (1 + math.Erf(x/math.Sqrt2))
}

// AutoCorrelation uses Pearson Correlation lagged by "lag".
func AutoCorrelation(x []float64, lag int) float64 {
	n := len(x)
	if lag >= n || lag <= 0 {
		return 0
	}
	return Correlation(x[:n-lag], x[lag:])
}

// SimpleOLS fits y = alpha + beta*x using least squares.
func SimpleOLS(x, y []float64) (alpha, beta float64) {
	if len(x) == 0 || len(x) != len(y) {
		return 0, 0
	}
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

// SpearmanCorrelation = Pearson correlation of ranks.
func SpearmanCorrelation(x, y []float64) float64 {
	if len(x) == 0 || len(x) != len(y) {
		return 0
	}
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

// CalcQuantileSpread: top-vs-bottom bucket mean return for a ranking signal.
func CalcQuantileSpread(sig, ret []float64, buckets int) float64 {
	n := len(sig)
	if n == 0 || n != len(ret) || buckets <= 1 {
		return 0
	}
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

// EstimateEntropy: Shannon entropy in bits, using fixed binning.
func EstimateEntropy(x []float64, bins int) float64 {
	n := len(x)
	if n == 0 || bins <= 1 {
		return 0
	}
	minV, maxV := x[0], x[0]
	for _, v := range x {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	if minV == maxV {
		return 0
	}
	hist := make([]int, bins)
	rng := maxV - minV
	for _, v := range x {
		idx := int(float64(bins) * (v - minV) / rng)
		if idx < 0 {
			idx = 0
		}
		if idx >= bins {
			idx = bins - 1
		}
		hist[idx]++
	}
	entropy := 0.0
	total := float64(n)
	for _, c := range hist {
		if c > 0 {
			p := float64(c) / total
			entropy -= p * math.Log2(p)
		}
	}
	return entropy
}

// EstimateHurst: simple R/S-based proxy for Hurst exponent over the full series.
func EstimateHurst(x []float64) float64 {
	n := len(x)
	if n < 10 {
		return 0.5
	}
	m := Mean(x)
	cumDev := 0.0
	maxCum, minCum := -1e9, 1e9
	ss := 0.0

	for _, v := range x {
		dev := v - m
		cumDev += dev
		if cumDev > maxCum {
			maxCum = cumDev
		}
		if cumDev < minCum {
			minCum = cumDev
		}
		ss += dev * dev
	}

	std := math.Sqrt(ss / float64(n))
	if std == 0 {
		return 0.5
	}
	rRange := maxCum - minCum
	return math.Log(rRange/std) / math.Log(float64(n))
}

// --- Persistence ------------------------------------------------------------

// SaveAlphaMetrics writes a slice of AlphaMetrics to a JSON file.
func SaveAlphaMetrics(path string, metrics []AlphaMetrics) error {
	if err := os.MkdirAll(filepathDir(path), 0755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(metrics)
}

// filepathDir is a tiny helper to avoid importing filepath just for Dir.
func filepathDir(path string) string {
	last := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			last = i
			break
		}
	}
	if last <= 0 {
		return "."
	}
	return path[:last]
}
