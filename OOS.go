package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// OOSStartDate defines the boundary between IS and OOS.
// All days < OOSStartDate are IS; >= OOSStartDate are OOS.
const OOSStartDate = "2024-01-01"

// ofiOOSAgg aggregates IS/OOS stats per variant.
type ofiOOSAgg struct {
	Variant string

	ISDays   int
	OOSDays  int
	ISBars   int64
	OOSBars  int64
	StartIS  string
	EndIS    string
	StartOOS string
	EndOOS   string

	IS_SumIC      float64
	OOS_SumIC     float64
	IS_SumHAC     float64
	OOS_SumHAC    float64
	IS_SumSR      float64
	OOS_SumSR     float64
	IS_SumProbSR  float64
	OOS_SumProbSR float64
	IS_SumBE      float64
	OOS_SumBE     float64
	IS_SumHit     float64
	OOS_SumHit    float64
	IS_SumFill    float64
	OOS_SumFill   float64
}

// runOOS reads the full OFI metrics report and produces a strict
// IS vs OOS comparison per variant at the configured TargetHz.
func runOOS() {
	// Parse OOS boundary once
	oosBoundary, err := time.Parse("2006-01-02", OOSStartDate)
	if err != nil {
		fmt.Printf("Invalid OOSStartDate const: %v\n", err)
		return
	}

	path := filepath.FromSlash(OFIReportPath)
	f, err := os.Open(path)
	if err != nil {
		fmt.Printf("Error opening OFI report: %v\n", err)
		return
	}
	defer f.Close()

	startT := time.Now()

	var entries []AlphaMetrics
	dec := json.NewDecoder(f)
	if err := dec.Decode(&entries); err != nil {
		fmt.Printf("Error decoding OFI report JSON: %v\n", err)
		return
	}
	if len(entries) == 0 {
		fmt.Println("No OFI metrics found.")
		return
	}

	aggMap := make(map[string]*ofiOOSAgg)

	for _, e := range entries {
		// label format: "SYMBOL|VARIANT|YYYY-MM-DD"
		parts := strings.Split(e.Label, "|")
		if len(parts) < 3 {
			continue
		}
		variant := parts[1]
		dateStr := parts[2]

		// parse date
		dayT, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}
		h, ok := e.Horizon[TargetHz]
		if !ok {
			continue
		}

		a, ok := aggMap[variant]
		if !ok {
			a = &ofiOOSAgg{Variant: variant}
			aggMap[variant] = a
		}

		isOOS := !dayT.Before(oosBoundary)

		if isOOS {
			a.OOSDays++
			a.OOSBars += int64(e.NBars)

			if a.StartOOS == "" || dateStr < a.StartOOS {
				a.StartOOS = dateStr
			}
			if a.EndOOS == "" || dateStr > a.EndOOS {
				a.EndOOS = dateStr
			}

			a.OOS_SumIC += h.ICPearson
			a.OOS_SumHAC += h.HACSharpe
			a.OOS_SumSR += h.TheoreticalSharpe
			a.OOS_SumProbSR += h.ProbSharpeRatio
			a.OOS_SumBE += h.BreakevenBps
			a.OOS_SumHit += h.DirectionalHit
			a.OOS_SumFill += h.FillRate
		} else {
			a.ISDays++
			a.ISBars += int64(e.NBars)

			if a.StartIS == "" || dateStr < a.StartIS {
				a.StartIS = dateStr
			}
			if a.EndIS == "" || dateStr > a.EndIS {
				a.EndIS = dateStr
			}

			a.IS_SumIC += h.ICPearson
			a.IS_SumHAC += h.HACSharpe
			a.IS_SumSR += h.TheoreticalSharpe
			a.IS_SumProbSR += h.ProbSharpeRatio
			a.IS_SumBE += h.BreakevenBps
			a.IS_SumHit += h.DirectionalHit
			a.IS_SumFill += h.FillRate
		}
	}

	if len(aggMap) == 0 {
		fmt.Println("No valid OFI metrics at target horizon for IS/OOS.")
		return
	}

	// Flatten and sort by OOS mean HAC Sharpe descending
	var aggs []*ofiOOSAgg
	for _, a := range aggMap {
		// Only keep variants that have at least some OOS coverage
		if a.OOSDays > 0 {
			aggs = append(aggs, a)
		}
	}
	if len(aggs) == 0 {
		fmt.Println("No variants have out-of-sample days after OOSStartDate.")
		return
	}

	sort.Slice(aggs, func(i, j int) bool {
		di := float64(aggs[i].OOSDays)
		dj := float64(aggs[j].OOSDays)
		if di == 0 || dj == 0 {
			return aggs[i].Variant < aggs[j].Variant
		}
		return (aggs[i].OOS_SumHAC / di) > (aggs[j].OOS_SumHAC / dj)
	})

	// Print summary
	fmt.Printf("\n--- OFI IS/OOS SUMMARY | %s | Horizon: %s ticks ---\n",
		filepath.Base(path), TargetHz)
	fmt.Printf("OOS boundary: %s (IS: < %s, OOS: >= %s)\n", OOSStartDate, OOSStartDate, OOSStartDate)
	fmt.Printf("Processing Time: %s\n\n", time.Since(startT))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	// Header
	fmt.Fprintf(w, "VARIANT\tIS_DAYS\tOOS_DAYS\tIS_HAC\tOOS_HAC\tOOS/IS\tIS_IC\tOOS_IC\tIS_ProbSR\tOOS_ProbSR\tIS_BE_BPS\tOOS_BE_BPS\tIS_HIT\tOOS_HIT\tIS_FILL\tOOS_FILL\tIS_PERIOD\tOOS_PERIOD\n")
	fmt.Fprintf(w, "-------\t-------\t--------\t------\t-------\t------\t-----\t------\t---------\t----------\t----------\t-----------\t------\t-------\t-------\t--------\t---------\t----------\n")

	for _, a := range aggs {
		isN := float64(a.ISDays)
		oosN := float64(a.OOSDays)

		var (
			isHAC, oosHAC, ratio float64
			isIC, oosIC          float64
			isProb, oosProb      float64
			isBE, oosBE          float64
			isHit, oosHit        float64
			isFill, oosFill      float64
		)

		if isN > 0 {
			isHAC = a.IS_SumHAC / isN
			isIC = a.IS_SumIC / isN
			isProb = a.IS_SumProbSR / isN
			isBE = a.IS_SumBE / isN
			isHit = (a.IS_SumHit / isN) * 100.0
			isFill = (a.IS_SumFill / isN) * 100.0
		}
		if oosN > 0 {
			oosHAC = a.OOS_SumHAC / oosN
			oosIC = a.OOS_SumIC / oosN
			oosProb = a.OOS_SumProbSR / oosN
			oosBE = a.OOS_SumBE / oosN
			oosHit = (a.OOS_SumHit / oosN) * 100.0
			oosFill = (a.OOS_SumFill / oosN) * 100.0
		}
		if isHAC != 0 {
			ratio = oosHAC / isHAC
		}

		fmt.Fprintf(
			w,
			"%s\t%d\t%d\t%.2f\t%.2f\t%.2f\t%.4f\t%.4f\t%.2f\t%.2f\t%.2f\t%.2f\t%.1f%%\t%.1f%%\t%.1f%%\t%.1f%%\t%s\t%s\n",
			a.Variant,
			a.ISDays,
			a.OOSDays,
			isHAC,
			oosHAC,
			ratio,
			isIC,
			oosIC,
			isProb,
			oosProb,
			isBE,
			oosBE,
			isHit,
			oosHit,
			isFill,
			oosFill,
			safePeriod(a.StartIS, a.EndIS),
			safePeriod(a.StartOOS, a.EndOOS),
		)
	}

	w.Flush()
	fmt.Println("")
}

// safePeriod formats "start→end" for date ranges, handling empty strings.
func safePeriod(start, end string) string {
	if start == "" && end == "" {
		return ""
	}
	if start == "" {
		return end
	}
	if end == "" {
		return start
	}
	return start + "→" + end
}
