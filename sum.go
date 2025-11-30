package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"
)

// --- Configuration ---
const (
	ReportPath = "data/reports/alpha_core_BTCUSDT.json"
	TargetHz   = "50" // We summarize the 50-tick horizon
)

// --- JSON Structures (Renamed to avoid conflicts) ---
// We use distinct names so they don't clash with scorecard.go
type reportEntry struct {
	Date          string                   `json:"date"`
	NBars         int                      `json:"n_bars"`
	SignalQuality reportQuality            `json:"signal_quality"`
	Horizons      map[string]reportHorizon `json:"horizons"`
}

type reportQuality struct {
	Turnover float64 `json:"est_turnover_per_bar"`
}

type reportHorizon struct {
	ICPearson         float64 `json:"ic_pearson"`
	BreakevenBps      float64 `json:"breakeven_cost_bps"`
	TheoreticalSharpe float64 `json:"theoretical_sharpe"`
	DecileSpreadBps   float64 `json:"decile_spread_bps"`
}

// runSum is the new entry point (renamed from main)
func runSum() {
	// 1. Setup Input
	path := filepath.FromSlash(ReportPath)
	f, err := os.Open(path)
	if err != nil {
		fmt.Printf("Error opening report: %v. Have you run 'study' yet?\n", err)
		return
	}
	defer f.Close()

	// 2. Streaming Decode (Memory Optimization)
	dec := json.NewDecoder(f)

	// Read opening bracket '['
	if _, err := dec.Token(); err != nil {
		// If file is empty or invalid JSON
		fmt.Println("Error: Invalid JSON format or empty report.")
		return
	}

	// 3. Aggregation State
	var (
		count       int
		totalBars   int64
		sumIC       float64
		sumBE       float64
		sumSharpe   float64
		sumSpread   float64
		sumTurnover float64
		posDays     int
		startDate   string
		endDate     string
	)

	startT := time.Now()

	// 4. Processing Loop
	for dec.More() {
		var res reportEntry
		if err := dec.Decode(&res); err != nil {
			break
		}

		if count == 0 {
			startDate = res.Date
		}
		endDate = res.Date

		// Extract metrics for the target horizon
		if h, ok := res.Horizons[TargetHz]; ok {
			count++
			totalBars += int64(res.NBars)

			sumIC += h.ICPearson
			sumBE += h.BreakevenBps
			sumSharpe += h.TheoreticalSharpe
			sumSpread += h.DecileSpreadBps
			sumTurnover += res.SignalQuality.Turnover

			if h.ICPearson > 0 {
				posDays++
			}
		}
	}

	// 5. Compute Final Stats
	if count == 0 {
		fmt.Println("No valid data found for horizon " + TargetHz)
		return
	}

	n := float64(count)
	avgIC := sumIC / n
	avgBE := sumBE / n
	avgSharpe := sumSharpe / n
	avgSpread := sumSpread / n
	avgTurnover := sumTurnover / n
	winRate := (float64(posDays) / n) * 100.0

	// 6. Output Generation
	fmt.Printf("\n--- ALPHA SUMMARY REPORT | %s ---\n", filepath.Base(path))
	fmt.Printf("Processing Time: %s | Target Horizon: %s ticks\n\n", time.Since(startT), TargetHz)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	// Section: General
	fmt.Fprintf(w, "METRIC\tVALUE\tDESCRIPTION\n")
	fmt.Fprintf(w, "---\t---\t---\n")
	fmt.Fprintf(w, "Date Range\t%s to %s\tScan duration\n", startDate, endDate)
	fmt.Fprintf(w, "Total Days\t%d\tValid trading days\n", count)
	fmt.Fprintf(w, "Total Bars\t%d\tTotal observations\n", totalBars)

	// Section: Performance
	fmt.Fprintf(w, "\nPERFORMANCE\t\t\n")
	fmt.Fprintf(w, "---\t\t\n")
	fmt.Fprintf(w, "Mean IC (Pearson)\t%.4f\tPredictive correlation\n", avgIC)
	fmt.Fprintf(w, "Mean Sharpe\t%.2f\tDaily theoretical sharpe\n", avgSharpe)
	fmt.Fprintf(w, "Win Rate (Days)\t%.1f%%\tPercent positive IC days\n", winRate)

	// Section: Costs & Execution
	fmt.Fprintf(w, "\nEXECUTION\t\t\n")
	fmt.Fprintf(w, "---\t\t\n")
	fmt.Fprintf(w, "Avg Breakeven\t%.2f bps\tCost required to kill alpha\n", avgBE)
	fmt.Fprintf(w, "Decile Spread\t%.2f bps\tTop minus Bottom decile return\n", avgSpread)
	fmt.Fprintf(w, "Avg Turnover\t%.2f\tEst. portfolio rotation per bar\n", avgTurnover)

	w.Flush()
	fmt.Println("")
}
