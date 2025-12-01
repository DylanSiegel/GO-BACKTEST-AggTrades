package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

func main() {
	// Hardware Optimization: Ryzen 9 7900X
	// Force the Go runtime to schedule goroutines across all logical threads.
	runtime.GOMAXPROCS(CPUThreads)

	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	start := time.Now()
	cmd := os.Args[1]

	switch cmd {
	case "data":
		runData() // Download + compress Binance data
	case "ofi":
		runOFI() // Mass-test OFI core directional variants
	case "sum":
		runSum() // Summarize OFI reports
	case "sanity":
		runSanity() // Verify file integrity
	case "oos":
		runOOS() // IS/OOS comparison for OFI variants
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		printHelp()
		os.Exit(1)
	}

	fmt.Printf("\n[sys] Execution Time: %s | Mem: %s\n", time.Since(start), getMemUsage())
}

func printHelp() {
	fmt.Println("Usage: quant.exe [command]")
	fmt.Println("--- DATA PIPELINE ---")
	fmt.Println("  data    - Download & compress raw aggTrades")
	fmt.Println("--- ALPHA LAB ---")
	fmt.Println("  ofi     - Test OFI core directional variants (15ms lag)")
	fmt.Println("  sum     - Summarize OFI metrics (variant ranking)")
	fmt.Println("--- AUDIT ---")
	fmt.Println("  sanity  - Verify data/index integrity")
}

func getMemUsage() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return fmt.Sprintf("%d MB", m.Alloc/1024/1024)
}
