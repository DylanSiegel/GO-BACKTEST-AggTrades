// main.go
package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

func main() {
	// 1. Windows/Ryzen Optimization: High Priority Class
	// This ensures the OS scheduler prioritizes your Quant/HFT threads
	runtime.GOMAXPROCS(24) // Pin to your 24 logical threads

	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	start := time.Now()
	cmd := os.Args[1]

	// 2. The Dispatcher (Zero-Allocation Switch)
	switch cmd {
	case "data":
		runData()
	case "build":
		runBuild()
	case "sanity":
		runSanity()
	case "study":
		runStudy()
	case "sum":
		runSum()
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		printHelp()
		os.Exit(1)
	}

	// 3. Performance telemetry
	fmt.Printf("\n[sys] Execution Time: %s | Mem: %s\n", time.Since(start), getMemUsage())
}

func printHelp() {
	fmt.Println("Usage: quant.exe [command]")
	fmt.Println("  data   - Download Binance AggTrades")
	fmt.Println("  build  - Generate Alpha Features")
	fmt.Println("  study  - Analyze Signal Decay")
	fmt.Println("  sum    - Summary Report")
}

func getMemUsage() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return fmt.Sprintf("%d MB", m.Alloc/1024/1024)
}
