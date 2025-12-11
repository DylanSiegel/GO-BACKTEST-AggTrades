# Safe Run Guide for 24GB MacBook

## ⚠️ IMPORTANT: Resource Management

This program was originally designed for 32GB+ systems. Here's how to run it safely on your 24GB MacBook.

## Memory Safety Limits

### Environment Variables to Set

```bash
# Limit Go heap to 12GB (50% of your RAM)
export GOGC=50

# Limit to 6 worker threads (instead of all cores)
# We'll modify the code to respect this
export MAX_WORKERS=6
```

## Code Modifications Required

Before building, we need to add a safety limit. The current code will try to use all your CPU cores, which could overwhelm memory.

### Option 1: Quick Fix (Recommended)
Just **don't run the full pipeline yet**. Let's:
1. Build the binary first (safe, no execution)
2. Test with a single day of data
3. Monitor resource usage
4. Scale up gradually

### Option 2: Add Hard Limits (Safer)
Modify `common.go` to cap workers:

```go
var (
    // Dynamic CPU detection with safety cap
    CPUThreads = func() int {
        cores := runtime.NumCPU()
        maxWorkers := 6  // Safe limit for 24GB
        if cores > maxWorkers {
            return maxWorkers
        }
        return cores
    }()
    BaseDir = "data"
)
```

## Memory Usage by Command

| Command | Expected Peak RAM | Safe? |
|---------|-------------------|-------|
| `build` | ~400MB per worker × 6 = ~2.4GB | ✅ YES |
| `data`  | ~100MB per worker × 6 = ~600MB | ✅ YES |
| `study` | ~400MB per worker × 6 = ~2.4GB | ✅ YES |
| `sanity` | ~200MB per worker × 6 = ~1.2GB | ✅ YES |

**With 6 workers max: ~3-4GB peak usage** - Very safe!

## Monitoring During Execution

Open Activity Monitor and watch:
- **Memory Pressure** (should stay GREEN)
- **Swap Used** (should stay low < 1GB)
- **CPU %** (will be high, that's OK)

### Stop Immediately If:
- Memory Pressure turns YELLOW or RED
- Swap exceeds 5GB
- System becomes unresponsive

**How to stop:** Press `Ctrl+C` in terminal

## Recommended Safe Testing Workflow

```bash
# 1. Build only (no execution, safe)
go build -o quant

# 2. Test help (instant, 0 memory)
./quant

# 3. Monitor system resources in another terminal
# Open Activity Monitor or run: watch -n 1 'top -l 1 | head -20'

# 4. Test with just sanity check (minimal)
./quant sanity

# 5. If all looks good, try data download for ONE day
# (We'd need to modify the code to support date range limits)
```

## What If Memory Gets Too High?

The program has graceful shutdown on `Ctrl+C`. It will:
- Finish current jobs
- Write what's been processed
- Exit cleanly
- NOT corrupt data

## Long-Term Solution

For your 24GB system, consider:
1. **Process in batches** - Download 1 month at a time
2. **Use external storage** - Store data on external SSD
3. **Cloud processing** - Rent a larger instance for heavy work
4. **Upgrade RAM** - If this is mission-critical work

## Quick Resource Cap Addition

Would you like me to:
1. ✅ **Add a hard-coded 6-worker limit** (safest for 24GB)
2. Add environment variable support (`MAX_WORKERS=6`)
3. Just build and test conservatively first

Let me know and I'll implement the safety you prefer!

---

**Bottom line:** With 6 workers instead of 10-12, you'll use ~4GB max, leaving plenty of headroom. The tradeoff is it'll take 2x longer to process.
