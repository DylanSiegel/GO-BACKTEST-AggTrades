package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

var Horizons = []int{20, 50, 100, 200}

func runStudy() {
	featDir := filepath.Join(BaseDir, "features", Symbol, fmt.Sprintf("%04d", TargetYear), fmt.Sprintf("%02d", TargetMonth))
	files, _ := os.ReadDir(featDir)

	type Job struct {
		Path string
		Name string
	}
	jobs := make(chan Job, len(files))
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".bin" {
			jobs <- Job{filepath.Join(featDir, f.Name()), f.Name()}
		}
	}
	close(jobs)

	var wg sync.WaitGroup
	results := make(chan DayResult, len(files))

	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results <- analyzeDay(j.Path, j.Name)
			}
		}()
	}
	wg.Wait()
	close(results)

	var allRes []DayResult
	for r := range results {
		allRes = append(allRes, r)
	}
	sort.Slice(allRes, func(i, j int) bool { return allRes[i].Name < allRes[j].Name })

	report := GlobalReport{
		Symbol:   Symbol,
		Year:     TargetYear,
		Month:    TargetMonth,
		Horizons: Horizons,
		Days:     make(map[string]DayResult),
	}

	fmt.Printf("%-5s | %10s | %8s | %8s\n", "DAY", "TICKS", "IC(20)", "SHARPE")
	fmt.Println("----------------------------------------")

	for _, r := range allRes {
		report.Days[r.Name] = r
		ic := 0.0
		if v, ok := r.ICTerm["20"]; ok {
			ic = v
		}
		fmt.Printf("%-5s | %10d | %8.4f | %8.2f\n", r.Name, r.Ticks, ic, r.Sharpe)
	}

	outPath := filepath.Join(BaseDir, "reports", "alpha_summary.json")
	os.MkdirAll(filepath.Dir(outPath), 0755)
	f, _ := os.Create(outPath)
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.Encode(report)
	f.Close()
	fmt.Println("\nJSON Written to", outPath)
}

type DayResult struct {
	Name   string
	Ticks  int
	ICTerm map[string]float64
	Sharpe float64
}

type GlobalReport struct {
	Symbol   string
	Year     int
	Month    int
	Horizons []int
	Days     map[string]DayResult
}

func analyzeDay(path, name string) DayResult {
	data, _ := os.ReadFile(path)
	n := len(data) / FeatureSize
	ts := make([]int64, n)
	px := make([]float64, n)
	sig := make([]float64, n)

	for i := 0; i < n; i++ {
		off := i * FeatureSize
		ts[i] = int64(binary.LittleEndian.Uint64(data[off:]))
		px[i] = math.Float64frombits(binary.LittleEndian.Uint64(data[off+8:]))
		sig[i] = math.Float64frombits(binary.LittleEndian.Uint64(data[off+16:]))
	}

	res := DayResult{Name: name, Ticks: n, ICTerm: make(map[string]float64)}
	if n < 500 {
		return res
	}

	for _, h := range Horizons {
		if n <= h {
			continue
		}
		var s, r []float64
		for i := 0; i < n-h; i++ {
			p0 := px[i]
			p1 := px[i+h]
			if p0 > 0 {
				ret := (p1 - p0) / p0
				s = append(s, sig[i])
				r = append(r, ret)
			}
		}
		res.ICTerm[fmt.Sprintf("%d", h)] = Correlation(s, r)
	}

	h := 20
	var pnl []float64
	for i := 0; i < n-h; i++ {
		p0 := px[i]
		p1 := px[i+h]
		if p0 > 0 && sig[i] != 0 {
			pnl = append(pnl, sig[i]*((p1-p0)/p0))
		}
	}

	if len(pnl) > 1 {
		mu := Mean(pnl)
		std := StdDev(pnl, mu)
		if std > 1e-9 {
			res.Sharpe = (mu / std) * math.Sqrt(365*24*60*6)
		}
	}
	return res
}
