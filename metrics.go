package main

import (
	"iter"
	"math"
	"sort"
)

type MetricStats struct {
	Count        int
	ICPearson    float64
	IC_TStat     float64 // Stability of IC across days
	Sharpe       float64
	HitRate      float64
	BreakevenBps float64
	AutoCorr     float64 // Lag-1 autocorrelation (mean-corrected)
}

type Moments struct {
	Count       float64
	SumSig      float64
	SumRet      float64
	SumProd     float64
	SumSqSig    float64
	SumSqRet    float64
	SumPnL      float64
	SumSqPnL    float64
	Hits        float64
	ValidHits   float64
	Turnover    float64
	SumProdLag  float64 // Σ(S_t * S_{t-1})
	SumSqSigLag float64 // Σ(S_{t-1}^2) (kept for completeness)
}

func (m *Moments) Add(m2 Moments) {
	m.Count += m2.Count
	m.SumSig += m2.SumSig
	m.SumRet += m2.SumRet
	m.SumProd += m2.SumProd
	m.SumSqSig += m2.SumSqSig
	m.SumSqRet += m2.SumSqRet
	m.SumPnL += m2.SumPnL
	m.SumSqPnL += m2.SumSqPnL
	m.Hits += m2.Hits
	m.ValidHits += m2.ValidHits
	m.Turnover += m2.Turnover
	m.SumProdLag += m2.SumProdLag
	m.SumSqSigLag += m2.SumSqSigLag
}

// Stream-based accumulation, used everywhere except quantile materialization.
func CalcMomentsStream(seq iter.Seq2[float64, float64]) Moments {
	var m Moments
	first := true
	prevSig := 0.0

	for s, r := range seq {
		m.Count++

		m.SumSig += s
		m.SumRet += r
		m.SumSqSig += s * s
		m.SumSqRet += r * r
		m.SumProd += s * r

		pnl := s * r
		m.SumPnL += pnl
		m.SumSqPnL += pnl * pnl

		if s != 0 && r != 0 {
			m.ValidHits++
			if (s > 0 && r > 0) || (s < 0 && r < 0) {
				m.Hits++
			}
		}

		if !first {
			d := s - prevSig
			if d < 0 {
				d = -d
			}
			m.Turnover += d

			m.SumProdLag += s * prevSig
			m.SumSqSigLag += prevSig * prevSig
		} else {
			first = false
		}
		prevSig = s
	}

	return m
}

// Slice-based variant used when we have already materialized (sig, ret) pairs
// for quantile analysis. Keeps math identical to CalcMomentsStream.
func CalcMomentsVectors(sigs, rets []float64) Moments {
	var m Moments
	n := len(sigs)
	if n == 0 {
		return m
	}
	prevSig := 0.0

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

		if s != 0 && r != 0 {
			m.ValidHits++
			if (s > 0 && r > 0) || (s < 0 && r < 0) {
				m.Hits++
			}
		}

		if i > 0 {
			d := s - prevSig
			if d < 0 {
				d = -d
			}
			m.Turnover += d
			m.SumProdLag += s * prevSig
			m.SumSqSigLag += prevSig * prevSig
		}
		prevSig = s
	}
	return m
}

func FinalizeMetrics(m Moments, dailyICs []float64) MetricStats {
	if m.Count <= 1 {
		return MetricStats{Count: int(m.Count)}
	}

	ms := MetricStats{Count: int(m.Count)}

	// 1. Pearson IC
	num := m.Count*m.SumProd - m.SumSig*m.SumRet
	denX := m.Count*m.SumSqSig - m.SumSig*m.SumSig
	denY := m.Count*m.SumSqRet - m.SumRet*m.SumRet
	if denX > 0 && denY > 0 {
		ms.ICPearson = num / math.Sqrt(denX*denY)
	}

	// 2. Sharpe on per-bar PnL
	meanPnL := m.SumPnL / m.Count
	varPnL := (m.SumSqPnL / m.Count) - (meanPnL * meanPnL)
	if varPnL > 0 {
		ms.Sharpe = meanPnL / math.Sqrt(varPnL)
	}

	// 3. Hit Rate
	if m.ValidHits > 0 {
		ms.HitRate = m.Hits / m.ValidHits
	}

	// 4. Breakeven bps per unit turnover
	if m.Turnover > 0 {
		ms.BreakevenBps = (m.SumPnL / m.Turnover) * 10000.0
	}

	// 5. Lag-1 Autocorrelation (mean-corrected)
	// We assume mean(S_t) ~ mean(S_{t-1}) for large N.
	// Cov_lag ≈ E[S_t S_{t-1}] - E[S]^2
	// Var(S)  = E[S^2] - E[S]^2
	if m.Count > 1 {
		meanSig := m.SumSig / m.Count
		covLag := (m.SumProdLag / m.Count) - (meanSig * meanSig)
		varSig := (m.SumSqSig / m.Count) - (meanSig * meanSig)

		if varSig > 1e-15 {
			ms.AutoCorr = covLag / varSig
		}
	}

	// 6. IC Stability (t-stat on daily ICs)
	if len(dailyICs) > 1 {
		var sum, sumSq float64
		for _, v := range dailyICs {
			sum += v
			sumSq += v * v
		}
		mean := sum / float64(len(dailyICs))
		variance := (sumSq / float64(len(dailyICs))) - (mean * mean)
		if variance > 0 {
			stdDev := math.Sqrt(variance)
			ms.IC_TStat = mean / (stdDev / math.Sqrt(float64(len(dailyICs))))
		}
	}

	return ms
}

// --- Quantile / Monotonicity math ---

type BucketResult struct {
	ID        int
	AvgSig    float64
	AvgRetBps float64
	Count     int
}

// ComputeQuantiles: sort (sig, ret) pairs and bucket by signal strength.
func ComputeQuantiles(sigs, rets []float64, numBuckets int) []BucketResult {
	n := len(sigs)
	if n == 0 || numBuckets <= 0 {
		return nil
	}

	type pair struct {
		s, r float64
	}
	pairs := make([]pair, n)
	for i := 0; i < n; i++ {
		pairs[i] = pair{s: sigs[i], r: rets[i]}
	}

	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].s < pairs[j].s
	})

	results := make([]BucketResult, numBuckets)
	bucketSize := n / numBuckets
	if bucketSize == 0 {
		bucketSize = 1
	}

	for b := 0; b < numBuckets; b++ {
		start := b * bucketSize
		end := start + bucketSize
		if b == numBuckets-1 || end > n {
			end = n
		}
		if start >= n {
			break
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
				Count:     count,
			}
		}
	}

	return results
}

// Aggregated bucket stats across many days.
type BucketAgg struct {
	Count     int
	SumSig    float64
	SumRetBps float64
}

func (ba *BucketAgg) Add(br BucketResult) {
	if br.Count == 0 {
		return
	}
	ba.Count += br.Count
	ba.SumSig += br.AvgSig * float64(br.Count)
	ba.SumRetBps += br.AvgRetBps * float64(br.Count)
}

func (ba BucketAgg) Finalize(id int) BucketResult {
	if ba.Count == 0 {
		return BucketResult{ID: id}
	}
	return BucketResult{
		ID:        id,
		AvgSig:    ba.SumSig / float64(ba.Count),
		AvgRetBps: ba.SumRetBps / float64(ba.Count),
		Count:     ba.Count,
	}
}
