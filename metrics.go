package main

import (
	"iter"
	"math"
	"sort"
)

// MetricStats is the finalized, human-readable view for one
// feature × horizon × sample (IS/OOS).
//
// Important: BreakevenBps is computed as:
//
//	BreakevenBps = 1e4 * SumPnL / SumAbsDeltaSig
//
// where:
//
//	SumPnL        = Σ S_t * R_t
//	SumAbsDeltaSig = Σ |S_t - S_{t-1}|
//
// Interpretation:
//   - S_t is treated as a target position in "units of notional".
//   - |S_t - S_{t-1}| is the notional TURNOVER per bar (one side).
//   - BreakevenBps is the GROSS alpha at MID per 1 unit of notional
//     traded (per side), in basis points.
//
// Trading-cost mapping:
//
//   - If your EFFECTIVE fee (including impact, spread, etc.) is F bps
//     PER SIDE of notional traded, then
//
//     NetBpsPerSide = BreakevenBps - F
//
//   - Break-even fee per side (max fee you can pay and still be flat)
//     is exactly:
//
//     F_max_per_side = BreakevenBps
//
//   - For a symmetric taker/taker model with per-side fee F_taker,
//     a full roundtrip pays 2 * F_taker, but turnover sees BOTH sides
//     separately, so you still compare BreakevenBps against F_taker
//     (per side), not against 2 * F_taker.
type MetricStats struct {
	Count int

	// Cross-sectional edge
	ICPearson float64 // Pearson IC over all aligned pairs
	IC_TStat  float64 // t-stat of daily ICs (stability)

	// PnL-quality
	Sharpe  float64 // Sharpe of bar-level signal * return
	HitRate float64 // Fraction of correct directional calls

	// Economic edge (per turnover)
	//
	// Gross alpha at mid per 1 unit of notional traded (per side),
	// in basis points. This is also the maximum fee PER SIDE (in bps)
	// that a pure taker strategy can pay and still break even.
	BreakevenBps float64

	// Signal dynamics
	AutoCorr    float64 // Corr(S_t, S_{t-1})
	AutoCorrAbs float64 // Corr(|S_t|, |S_{t-1}|)

	AvgSegLen float64 // Average run length of same-sign segments (bars)
	MaxSegLen float64 // Maximum observed same-sign run length (bars)
}

// Moments is the streaming accumulator over aligned pairs (S,R).
// Multiple days are merged by .Add(), and then finalized by FinalizeMetrics.
//
// Interpretation for trading economics:
//
//   - S_t: signal interpreted as target position in "units of notional".
//   - R_t: forward return over horizon (fractional, e.g. 0.001 = 10 bps).
//   - PnL_t = S_t * R_t (per bar).
//   - SumAbsDeltaSig = Σ |S_t - S_{t-1}| is total notional turnover (per side).
//
// Under this model, BreakevenBps = 1e4 * SumPnL / SumAbsDeltaSig
// is the GROSS alpha at mid per 1 unit of notional traded per side.
type Moments struct {
	Count float64

	// Signal/return stats
	SumSig   float64
	SumRet   float64
	SumProd  float64
	SumSqSig float64
	SumSqRet float64

	// PnL stats (per aligned bar)
	SumPnL   float64
	SumSqPnL float64

	// Direction accuracy
	Hits      float64
	ValidHits float64

	// Turnover proxy (per-side notional traded)
	SumAbsDeltaSig float64 // Σ |S_t - S_{t-1}|

	// Autocorrelation (raw signal)
	SumProdLag float64 // Σ S_t * S_{t-1}

	// Autocorrelation (abs signal)
	SumAbsSig     float64 // Σ |S_t|
	SumAbsProdLag float64 // Σ |S_t| * |S_{t-1}|

	// Segment statistics (runs of same sign)
	SegCount    float64 // number of segments (non-zero sign runs)
	SegLenTotal float64 // Σ length of segments in bars
	SegLenMax   float64 // max segment length in bars
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

	m.SumAbsDeltaSig += m2.SumAbsDeltaSig
	m.SumProdLag += m2.SumProdLag

	m.SumAbsSig += m2.SumAbsSig
	m.SumAbsProdLag += m2.SumAbsProdLag

	m.SegCount += m2.SegCount
	m.SegLenTotal += m2.SegLenTotal
	if m2.SegLenMax > m.SegLenMax {
		m.SegLenMax = m2.SegLenMax
	}
}

// CalcMomentsStream: streaming version from an iterator (S,R).
// Used where we don't need to materialize slices.
func CalcMomentsStream(seq iter.Seq2[float64, float64]) Moments {
	var m Moments

	first := true
	var prevSig float64
	var prevSign float64
	var curSegLen float64

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

		absS := s
		if absS < 0 {
			absS = -absS
		}
		m.SumAbsSig += absS

		// hit-rate: directional correctness
		if s != 0 && r != 0 {
			m.ValidHits++
			if (s > 0 && r > 0) || (s < 0 && r < 0) {
				m.Hits++
			}
		}

		// dynamic metrics: turnover, autocorr, segments
		if !first {
			// turnover (per-side notional)
			d := s - prevSig
			if d < 0 {
				d = -d
			}
			m.SumAbsDeltaSig += d

			// lag-1 signal autocorr
			m.SumProdLag += s * prevSig

			// lag-1 abs(signal) autocorr
			absPrev := prevSig
			if absPrev < 0 {
				absPrev = -absPrev
			}
			m.SumAbsProdLag += absS * absPrev
		} else {
			first = false
		}

		// segment statistics: runs of same non-zero sign
		sign := 0.0
		if s > 0 {
			sign = 1.0
		} else if s < 0 {
			sign = -1.0
		}

		if sign != 0 {
			if prevSign == sign {
				// continuing segment
				curSegLen++
			} else {
				// closing previous segment if any
				if curSegLen > 0 {
					m.SegCount++
					m.SegLenTotal += curSegLen
					if curSegLen > m.SegLenMax {
						m.SegLenMax = curSegLen
					}
				}
				// start new segment
				curSegLen = 1
			}
		} else {
			// zero signal closes any current segment
			if curSegLen > 0 {
				m.SegCount++
				m.SegLenTotal += curSegLen
				if curSegLen > m.SegLenMax {
					m.SegLenMax = curSegLen
				}
				curSegLen = 0
			}
		}

		prevSig = s
		prevSign = sign
	}

	// flush last open segment
	if curSegLen > 0 {
		m.SegCount++
		m.SegLenTotal += curSegLen
		if curSegLen > m.SegLenMax {
			m.SegLenMax = curSegLen
		}
	}

	return m
}

// CalcMomentsVectors: same math as CalcMomentsStream, but operating on
// already-materialized slices of equal length.
func CalcMomentsVectors(sigs, rets []float64) Moments {
	var m Moments
	n := len(sigs)
	if n == 0 {
		return m
	}

	var prevSig float64
	var prevSign float64
	var curSegLen float64

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

		absS := s
		if absS < 0 {
			absS = -absS
		}
		m.SumAbsSig += absS

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
			m.SumAbsDeltaSig += d

			m.SumProdLag += s * prevSig

			absPrev := prevSig
			if absPrev < 0 {
				absPrev = -absPrev
			}
			m.SumAbsProdLag += absS * absPrev
		}

		sign := 0.0
		if s > 0 {
			sign = 1.0
		} else if s < 0 {
			sign = -1.0
		}

		if sign != 0 {
			if prevSign == sign {
				curSegLen++
			} else {
				if curSegLen > 0 {
					m.SegCount++
					m.SegLenTotal += curSegLen
					if curSegLen > m.SegLenMax {
						m.SegLenMax = curSegLen
					}
				}
				curSegLen = 1
			}
		} else {
			if curSegLen > 0 {
				m.SegCount++
				m.SegLenTotal += curSegLen
				if curSegLen > m.SegLenMax {
					m.SegLenMax = curSegLen
				}
				curSegLen = 0
			}
		}

		prevSig = s
		prevSign = sign
	}

	if curSegLen > 0 {
		m.SegCount++
		m.SegLenTotal += curSegLen
		if curSegLen > m.SegLenMax {
			m.SegLenMax = curSegLen
		}
	}

	return m
}

// FinalizeMetrics turns low-level Moments + daily IC series into a compact
// statistical view (MetricStats).
func FinalizeMetrics(m Moments, dailyICs []float64) MetricStats {
	if m.Count <= 1 {
		return MetricStats{Count: int(m.Count)}
	}

	ms := MetricStats{Count: int(m.Count)}

	// 1. Pearson IC over all aligned pairs.
	num := m.Count*m.SumProd - m.SumSig*m.SumRet
	denX := m.Count*m.SumSqSig - m.SumSig*m.SumSig
	denY := m.Count*m.SumSqRet - m.SumRet*m.SumRet
	if denX > 0 && denY > 0 {
		ms.ICPearson = num / math.Sqrt(denX*denY)
	}

	// 2. Sharpe of bar-level signal * return.
	meanPnL := m.SumPnL / m.Count
	varPnL := (m.SumSqPnL / m.Count) - meanPnL*meanPnL
	if varPnL > 1e-18 {
		ms.Sharpe = meanPnL / math.Sqrt(varPnL)
	}

	// 3. Hit rate.
	if m.ValidHits > 0 {
		ms.HitRate = m.Hits / m.ValidHits
	}

	// 4. Breakeven bps per unit turnover (per-side notional):
	//    gross alpha at mid per 1 unit of notional traded, in bps.
	if m.SumAbsDeltaSig > 1e-18 {
		ms.BreakevenBps = (m.SumPnL / m.SumAbsDeltaSig) * 10000.0
	}

	// 5. Lag-1 autocorrelation of S_t (mean-corrected).
	meanSig := m.SumSig / m.Count
	covLag := (m.SumProdLag / m.Count) - meanSig*meanSig
	varSig := (m.SumSqSig / m.Count) - meanSig*meanSig
	if varSig > 1e-18 {
		ms.AutoCorr = covLag / varSig
	}

	// 6. Lag-1 autocorrelation of |S_t|.
	if m.Count > 0 {
		meanAbs := m.SumAbsSig / m.Count
		covAbs := (m.SumAbsProdLag / m.Count) - meanAbs*meanAbs
		// var(|S|) uses same SumSqSig (since |S|^2 == S^2)
		varAbs := (m.SumSqSig / m.Count) - meanAbs*meanAbs
		if varAbs > 1e-18 {
			ms.AutoCorrAbs = covAbs / varAbs
		}
	}

	// 7. Segment statistics: average/max run length of same-sign signal.
	if m.SegCount > 0 {
		ms.AvgSegLen = m.SegLenTotal / m.SegCount
	}
	ms.MaxSegLen = m.SegLenMax

	// 8. IC Stability: t-stat of daily ICs.
	if len(dailyICs) > 1 {
		var sum, sumSq float64
		n := float64(len(dailyICs))
		for _, v := range dailyICs {
			sum += v
			sumSq += v * v
		}
		mean := sum / n
		variance := (sumSq / n) - mean*mean
		if variance > 1e-18 {
			stdDev := math.Sqrt(variance)
			ms.IC_TStat = mean / (stdDev / math.Sqrt(n))
		}
	}

	return ms
}

// --- Quantile / Monotonicity Math ---

type BucketResult struct {
	ID        int
	AvgSig    float64
	AvgRetBps float64
	Count     int
}

// ComputeQuantiles sorts (sig, ret) pairs by signal and splits into
// numBuckets groups, returning per-bucket statistics.
func ComputeQuantiles(sigs, rets []float64, numBuckets int) []BucketResult {
	n := len(sigs)
	if n == 0 || numBuckets <= 0 || len(rets) != n {
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

// BucketAgg aggregates bucket results across days.
type BucketAgg struct {
	Count     int
	SumSig    float64
	SumRetBps float64
}

func (ba *BucketAgg) Add(br BucketResult) {
	if br.Count <= 0 {
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
	den := float64(ba.Count)
	return BucketResult{
		ID:        id,
		AvgSig:    ba.SumSig / den,
		AvgRetBps: ba.SumRetBps / den,
		Count:     ba.Count,
	}
}
