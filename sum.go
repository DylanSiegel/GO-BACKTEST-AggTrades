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

const (
	// OFI report produced by runOFI()
	OFIReportPath = "data/reports/ofi_BTCUSDT.json"

	// Target horizon key inside AlphaMetrics.Horizon (as set in ofi.go)
	TargetHz = "50"
)

// Aggregation container per OFI variant
type ofiAgg struct {
	Variant   string
	Days      int
	TotalBars int64
	StartDate string
	EndDate   string

	SumIC       float64
	SumHAC      float64
	SumSharpe   float64
	SumProbSR   float64
	SumBE       float64
	SumFill     float64
	SumHit      float64
	SumHalfBars float64
	SumHalfMs   float64
	PosICDays   int
	PosHACDays  int
}

// runSum summarizes OFI metrics across all days and variants at TargetHz.
func runSum() {
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

	// Aggregate per variant
	aggMap := make(map[string]*ofiAgg)

	for _, e := range entries {
		// label format: "SYMBOL|VARIANT|YYYY-MM-DD"
		parts := strings.Split(e.Label, "|")
		if len(parts) < 3 {
			continue
		}
		variant := parts[1]
		date := parts[2]

		h, ok := e.Horizon[TargetHz]
		if !ok {
			continue
		}

		a, ok := aggMap[variant]
		if !ok {
			a = &ofiAgg{Variant: variant}
			aggMap[variant] = a
		}

		a.Days++
		a.TotalBars += int64(e.NBars)

		if a.StartDate == "" || date < a.StartDate {
			a.StartDate = date
		}
		if a.EndDate == "" || date > a.EndDate {
			a.EndDate = date
		}

		a.SumIC += h.ICPearson
		a.SumHAC += h.HACSharpe
		a.SumSharpe += h.TheoreticalSharpe
		a.SumProbSR += h.ProbSharpeRatio
		a.SumBE += h.BreakevenBps
		a.SumFill += h.FillRate
		a.SumHit += h.DirectionalHit
		a.SumHalfBars += h.AlphaHalfLifeBars
		a.SumHalfMs += h.AlphaHalfLifeMs

		if h.ICPearson > 0 {
			a.PosICDays++
		}
		if h.HACSharpe > 0 {
			a.PosHACDays++
		}
	}

	if len(aggMap) == 0 {
		fmt.Println("No valid OFI metrics at target horizon.")
		return
	}

	// Convert to slice and rank by mean HAC Sharpe
	var aggs []*ofiAgg
	for _, a := range aggMap {
		aggs = append(aggs, a)
	}

	sort.Slice(aggs, func(i, j int) bool {
		di := float64(aggs[i].Days)
		dj := float64(aggs[j].Days)
		if di == 0 || dj == 0 {
			return aggs[i].Variant < aggs[j].Variant
		}
		return (aggs[i].SumHAC / di) > (aggs[j].SumHAC / dj)
	})

	// Output summary
	fmt.Printf("\n--- OFI VARIANT SUMMARY | %s | Horizon: %s ticks ---\n",
		filepath.Base(path), TargetHz)
	fmt.Printf("Processing Time: %s\n\n", time.Since(startT))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	// Header
	fmt.Fprintf(w, "VARIANT\tDAYS\tBARS\tMEAN_IC\tHIT_RATE\tMEAN_HAC\tMEAN_SR\tBREAKEVEN_BPS\tFILL_RATE\tHALF_LIFE_MS\tPOS_IC%%\tPOS_HAC%%\tPERIOD\n")
	fmt.Fprintf(w, "------\t----\t----\t-------\t--------\t--------\t-------\t-------------\t---------\t------------\t-------\t--------\t------\n")

	for _, a := range aggs {
		if a.Days == 0 {
			continue
		}
		n := float64(a.Days)

		meanIC := a.SumIC / n
		meanHAC := a.SumHAC / n
		meanSR := a.SumSharpe / n
		meanBE := a.SumBE / n
		meanFill := (a.SumFill / n) * 100.0
		meanHit := (a.SumHit / n) * 100.0
		meanHLms := a.SumHalfMs / n

		posICPct := 100.0 * float64(a.PosICDays) / n
		posHACPct := 100.0 * float64(a.PosHACDays) / n

		fmt.Fprintf(w, "%s\t%d\t%d\t%.4f\t%.1f%%\t%.2f\t%.2f\t%.2f\t%.1f%%\t%.1f\t%.1f%%\t%.1f%%\t%s→%s\n",
			a.Variant,
			a.Days,
			a.TotalBars,
			meanIC,
			meanHit,
			meanHAC,
			meanSR,
			meanBE,
			meanFill,
			meanHLms,
			posICPct,
			posHACPct,
			a.StartDate,
			a.EndDate,
		)
	}

	w.Flush()
	fmt.Println("")
}
