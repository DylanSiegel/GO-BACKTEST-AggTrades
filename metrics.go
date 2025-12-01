package main

import (
	"math"
)

// MetricStats holds the metrics actually used by the study path.
type MetricStats struct {
	Count        int
	ICPearson    float64 // linear IC
	Sharpe       float64 // per-trade Sharpe
	SharpeAnnual float64 // scaled Sharpe (sqrt(N) over sample)
	HitRate      float64 // directional accuracy (non-zero signal & return)
	BreakevenBps float64 // break-even cost per unit turnover (bps)
}

// ComputeStats calculates IC, Sharpe, hit rate, and break-even cost
// for a signal vs return series. sig and ret must be aligned and of
// equal length. This function is called very frequently in the study
// pipeline and is written to be single-pass and branch-light.
func ComputeStats(sig, ret []float64) MetricStats {
	n := len(sig)
	if n < 2 {
		return MetricStats{}
	}

	ms := MetricStats{Count: n}

	var (
		sumSig, sumRet     float64
		sumSqSig, sumSqRet float64
		sumProd            float64

		// PnL aggregates
		sumPnL, sumSqPnL float64

		// Hit-rate aggregates
		hits, validHits float64

		// Turnover
		turnover float64
		prevSig  float64
	)

	for i := 0; i < n; i++ {
		s := sig[i]
		r := ret[i]

		// Moments for Pearson IC
		sumSig += s
		sumRet += r
		sumSqSig += s * s
		sumSqRet += r * r
		sumProd += s * r

		// PnL stats
		pnl := s * r
		sumPnL += pnl
		sumSqPnL += pnl * pnl

		// Hit rate (directional)
		if s != 0 && r != 0 {
			validHits++
			if (s > 0 && r > 0) || (s < 0 && r < 0) {
				hits++
			}
		}

		// Turnover (L1 change of signal)
		if i > 0 {
			turnover += math.Abs(s - prevSig)
		}
		prevSig = s
	}

	fn := float64(n)

	// 1) Pearson IC
	num := fn*sumProd - sumSig*sumRet
	denX := fn*sumSqSig - sumSig*sumSig
	denY := fn*sumSqRet - sumRet*sumRet
	if denX > 0 && denY > 0 {
		ms.ICPearson = num / math.Sqrt(denX*denY)
	}

	// 2) Sharpe per trade and "annualized" via sqrt(N) over sample.
	meanPnL := sumPnL / fn
	varPnL := (sumSqPnL / fn) - (meanPnL * meanPnL)
	if varPnL > 0 {
		stdPnL := math.Sqrt(varPnL)
		sh := meanPnL / stdPnL
		ms.Sharpe = sh
		ms.SharpeAnnual = sh * math.Sqrt(fn)
	}

	// 3) Hit rate
	if validHits > 0 {
		ms.HitRate = hits / validHits
	}

	// 4) Break-even cost in bps per unit turnover.
	if turnover > 0 {
		ms.BreakevenBps = (sumPnL / turnover) * 10000.0
	}

	return ms
}
