package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

func main() {
	// Set GOMAXPROCS to utilize all M4 cores (P+E)
	runtime.GOMAXPROCS(CPUThreads)

	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	start := time.Now()

	fmt.Printf("Apple Silicon Quant Pipeline | Go 1.25+ | OS: %s/%s | Cores: %d\n",
		runtime.GOOS, runtime.GOARCH, CPUThreads)
	
	// Check for common ARM64 vars
	if gogc := os.Getenv("GOGC"); gogc != "" {
		fmt.Printf("Env GOGC: %s\n", gogc)
	}

	cmd := os.Args[1]

	switch cmd {
	case "data":
		runData()
	case "build":
		runBuild()
	case "study":
		runStudy()
	case "sanity":
		runSanity()
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		printHelp()
		os.Exit(1)
	}

	fmt.Printf("\n[sys] Execution Time: %s | Mem: %s\n", time.Since(start), getMemUsage())
}

func printHelp() {
	fmt.Println("Usage: quant [command]")
	fmt.Println("  data   - Download raw aggTrades")
	fmt.Println("  build  - Run Hawkes/Adaptive/EMA models -> features")
	fmt.Println("  study  - Run IS/OOS backtest on features")
	fmt.Println("  sanity - Check data integrity")
}

func getMemUsage() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return fmt.Sprintf("%d MB", m.Alloc/1024/1024)
}
