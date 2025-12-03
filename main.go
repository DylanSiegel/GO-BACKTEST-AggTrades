package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

func main() {
	// Use all 7900X hardware threads.
	runtime.GOMAXPROCS(CPUThreads)

	// Hard memory limit: ~30GB heap.
	// This is a guardrail on top of GOGC=200, not a replacement.
	const ramLimit = 30 * 1024 * 1024 * 1024
	debug.SetMemoryLimit(ramLimit)

	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	start := time.Now()

	fmt.Printf("Go 1.25.4 | Env: %s/%s | Threads: %d | RAM Limit: 30GB | GOGC: %s | AMD64: %s\n",
		runtime.GOOS, runtime.GOARCH,
		runtime.GOMAXPROCS(0),
		os.Getenv("GOGC"),
		os.Getenv("GOAMD64"))

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
	fmt.Println("Usage: quant.exe [command]")
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
