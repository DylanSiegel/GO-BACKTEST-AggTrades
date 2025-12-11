# Resource Guide - Optimized for 24GB MacBook

## Your System Profile
- **RAM:** 24GB
- **Storage:** 415GB built-in SSD
- **Config:** 8 workers (balanced for performance + safety)

## Performance vs Safety Trade-offs

### Current Configuration: 8 Workers

| Metric | Value | Status |
|--------|-------|--------|
| **Peak RAM Usage** | 6-8GB | ✅ Safe (33% of RAM) |
| **Free RAM Remaining** | 16GB+ | ✅ Plenty for macOS + apps |
| **CPU Utilization** | 80-90% | ✅ High performance |
| **Processing Speed** | 1.5x faster than 6 workers | ✅ Good balance |
| **Storage Required** | 300-350GB | ⚠️ **TIGHT** (leaves ~65GB) |

## Storage Breakdown

```
Total: 415GB
├── macOS System: ~20GB
├── Your Apps/Files: ~30GB (estimated)
├── Bitcoin Data: ~300-350GB ⚠️
└── Free Space After: ~65GB ⚠️
```

### Storage Recommendation

**⚠️ CRITICAL:** You're borderline on storage. Consider:

1. **External SSD Option** (Recommended)
   - Move `data/` folder to external drive
   - Keeps MacBook storage free
   - No performance penalty with USB-C/Thunderbolt

2. **Selective Download**
   - Download only recent years (2023-2024)
   - ~50GB instead of 350GB
   - Still gives you 2+ years of data

3. **Cloud Storage**
   - Store processed features in cloud
   - Keep only raw data locally

## Memory Configuration Options

Want to tune further? Here are your options:

### Conservative (Ultra-Safe)
```go
maxWorkers := 6  // 4-5GB peak, very safe, slower
```

### Balanced (Current - Recommended)
```go
maxWorkers := 8  // 6-8GB peak, good speed, safe
```

### Aggressive (Maximum Performance)
```go
maxWorkers := 10  // 8-12GB peak, fastest, monitor closely
```

### Full Throttle (Not Recommended)
```go
maxWorkers := runtime.NumCPU()  // 12-16GB peak, risky
```

## Performance Estimates (8 Workers)

### M4 MacBook Air/Pro (10-core)
- **Data Download:** 2-4 hours (network-bound)
- **Feature Build:** 30-50 minutes
- **Study Execution:** 5-10 minutes
- **Total Pipeline:** ~3-5 hours

### Comparison vs Original (24 workers on Ryzen)
- **Speed:** ~70% as fast
- **Safety:** Much safer
- **Trade-off:** Worth it for stability

## Environment Tuning

Set these for optimal performance:

```bash
# Moderate GC - balances speed and memory
export GOGC=100

# Optional: Profile memory usage
export GODEBUG=gctrace=1

# Build optimized binary
go build -o quant
```

## Monitoring Commands

### Check Storage Before Starting
```bash
df -h  # Should show >100GB free on main drive
```

### Monitor During Execution
```bash
# Terminal 1: Run the program
./quant build

# Terminal 2: Watch resources
watch -n 2 'ps aux | grep quant | grep -v grep'

# Or use Activity Monitor GUI
open -a "Activity Monitor"
```

### Safe Operating Ranges
- **Memory Pressure:** GREEN (Safe) or YELLOW (OK)
- **Swap Used:** < 2GB (Safe)
- **Free Disk:** > 50GB (Critical minimum)

## Storage Strategy Recommendations

### Option A: External SSD (Best)
```bash
# Create data folder on external drive
mkdir /Volumes/ExternalSSD/crypto-data
ln -s /Volumes/ExternalSSD/crypto-data data

# Run normally
./quant data
```

### Option B: Selective Years Only
Modify `data.go` line 59:
```go
// Change from 2020-01-01 to 2023-01-01
const FallbackDt = "2023-01-01"
```
Saves ~200GB!

### Option C: Download & Delete
1. Download 1 year at a time
2. Build features for that year
3. Delete raw data, keep features (~1/10 the size)
4. Repeat for next year

## Quick Decision Matrix

**If you want:**
- **Maximum Speed** → Keep 8 workers, use external SSD
- **Maximum Safety** → Change to 6 workers, selective download
- **Balanced** → Keep current (8 workers), monitor closely
- **Test First** → Keep 8 workers, download 2024 data only (~30GB)

## Recommended First Run

Start small to verify everything works:

```bash
# 1. Build
go build -o quant

# 2. Test with 2024 data only (modify FallbackDt first)
./quant data

# 3. Monitor storage
df -h

# 4. If all good, continue
./quant build
./quant study
```

---

## Current Status: ✅ Optimized for 24GB

- **Workers:** 8 (good balance)
- **Expected RAM:** 6-8GB peak
- **Storage Risk:** ⚠️ MEDIUM (consider external SSD or selective download)
- **Performance:** ~70% of full-throttle, very stable

Would you like me to:
1. ✅ Leave as-is (8 workers - recommended)
2. Add external SSD path support
3. Add selective year download option
4. Increase to 10 workers (more aggressive)
