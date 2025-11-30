package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
)

// --- Configuration ---
const (
	Warmup = 5000
)

// --- Precomputed Kernels ---
var (
	PropResKernel [2048]float64
	CountPowLook  [256]float64
	FragDecay     = math.Exp(-0.09)
)

func init() {
	// Pre-compute expensive powers
	for k := 0; k < 2048; k++ {
		PropResKernel[k] = 1.0 / math.Pow(float64(k+12), 0.41)
	}
	for c := 0; c < 256; c++ {
		// Clamped power curve for trade counts
		CountPowLook[c] = math.Min(math.Pow(float64(c), 0.63), 8.8)
	}
}

// --- Entry Point ---
func runBuild() {
	fmt.Printf("--- BUILDALPHA GO (Adaptive Z-Score) | FULL HISTORY SCAN: %s ---\n", Symbol)

	// 1. Discovery Phase
	root := filepath.Join(BaseDir, Symbol)
	var tasks []TaskID

	years, _ := os.ReadDir(root)
	for _, yDir := range years {
		if !yDir.IsDir() {
			continue
		}
		y, err := strconv.Atoi(yDir.Name())
		if err != nil {
			continue
		}

		months, _ := os.ReadDir(filepath.Join(root, yDir.Name()))
		for _, mDir := range months {
			if !mDir.IsDir() {
				continue
			}
			m, err := strconv.Atoi(mDir.Name())
			if err != nil {
				continue
			}

			// Check for index file existence to confirm valid data
			idxPath := filepath.Join(root, yDir.Name(), mDir.Name(), "index.quantdev")
			if _, err := os.Stat(idxPath); err == nil {
				// Add days 1-31 (existence checked later)
				for d := 1; d <= 31; d++ {
					tasks = append(tasks, TaskID{y, m, d})
				}
			}
		}
	}

	// Sort tasks for chronological execution
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Y != tasks[j].Y {
			return tasks[i].Y < tasks[j].Y
		}
		if tasks[i].M != tasks[j].M {
			return tasks[i].M < tasks[j].M
		}
		return tasks[i].D < tasks[j].D
	})

	fmt.Printf("[build] Found potential data for %d months. Starting build...\n", len(tasks)/31)

	// 2. Execution Phase
	// Note: We process days sequentially because processBuildDay internally
	// parallelizes across all CPUThreads using the 'chunks' logic.
	// Running days in parallel + chunks in parallel would cause thrashing.
	validDays := 0
	for _, t := range tasks {
		res, built := processBuildDay(t.Y, t.M, t.D)
		if built {
			fmt.Println(res)
			validDays++
		}
	}
	fmt.Printf("--- Build Complete. Built %d days of Alpha. ---\n", validDays)
}

type TaskID struct {
	Y, M, D int
}

// --- Online Normalization (Welford's Algorithm) ---
type OnlineZ struct {
	count float64
	mean  float64
	m2    float64
}

// Updates stats and returns the Z-Score of x
func (z *OnlineZ) Update(x float64) float64 {
	z.count++
	delta := x - z.mean
	z.mean += delta / z.count
	delta2 := x - z.mean
	z.m2 += delta * delta2

	if z.count < 200 {
		return 0.0 // Warmup period for the Z-score itself
	}

	// Variance = m2 / (count - 1)
	variance := z.m2 / (z.count - 1)
	if variance < 1e-12 {
		return 0.0
	}

	std := math.Sqrt(variance)
	val := (x - z.mean) / std

	// Clamp to +/- 4.0 to prevent blown-up outliers from ruining the linear combo
	if val > 4.0 {
		return 4.0
	}
	if val < -4.0 {
		return -4.0
	}
	return val
}

// --- Pipeline ---

// Returns: Status String, Built Boolean
func processBuildDay(year, month, day int) (string, bool) {
	dir := filepath.Join(BaseDir, Symbol, fmt.Sprintf("%04d", year), fmt.Sprintf("%02d", month))
	idxPath := filepath.Join(dir, "index.quantdev")
	dataPath := filepath.Join(dir, "data.quantdev")

	offset, length := findBlob(idxPath, day)
	if length == 0 {
		// Silent return for non-existent days (e.g. Feb 30)
		return "", false
	}

	// 1. IO Read
	f, err := os.Open(dataPath)
	if err != nil {
		return fmt.Sprintf("ERR_IO %04d-%02d-%02d", year, month, day), false
	}
	defer f.Close()
	f.Seek(int64(offset), 0)
	compData := make([]byte, length)
	f.Read(compData)

	// 2. Decompress
	r, err := zlib.NewReader(bytes.NewReader(compData))
	if err != nil {
		return fmt.Sprintf("ERR_ZLIB %04d-%02d-%02d", year, month, day), false
	}
	blob, err := io.ReadAll(r)
	r.Close()

	if len(blob) < HeaderSize {
		return "ERR_HDR", false
	}

	// 3. Header Parse
	rowCount := binary.LittleEndian.Uint64(blob[8:])
	body := blob[HeaderSize:]

	if uint64(len(body)) != rowCount*RowSize {
		return "ERR_SIZE", false
	}

	// 4. Parallel Processing
	// Splitting the row processing across cores for this specific day
	chunks := buildChunks(int(rowCount), CPUThreads)
	results := make([][]byte, CPUThreads)
	var wg sync.WaitGroup

	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func(idx int, start, end int) {
			defer wg.Done()
			results[idx] = processKernel(body, start, end, Warmup)
		}(i, chunks[i][0], chunks[i][1])
	}
	wg.Wait()

	// 5. Merge & Write
	var outBuf bytes.Buffer
	for _, res := range results {
		outBuf.Write(res)
	}

	outDir := filepath.Join(BaseDir, "features", Symbol, fmt.Sprintf("%04d", year), fmt.Sprintf("%02d", month))
	os.MkdirAll(outDir, 0755)
	outPath := filepath.Join(outDir, fmt.Sprintf("%02d.bin", day))
	os.WriteFile(outPath, outBuf.Bytes(), 0644)

	return fmt.Sprintf("DONE %04d-%02d-%02d | %d rows", year, month, day, rowCount), true
}

func buildChunks(total, n int) [][2]int {
	res := make([][2]int, n)
	base, rem := total/n, total%n
	start := 0
	for i := 0; i < n; i++ {
		len := base
		if i < rem {
			len++
		}
		res[i] = [2]int{start, start + len}
		start += len
	}
	return res
}

func findBlob(idxPath string, targetDay int) (uint64, uint64) {
	f, err := os.Open(idxPath)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	hdr := make([]byte, 16)
	f.Read(hdr)
	count := binary.LittleEndian.Uint64(hdr[8:])
	row := make([]byte, 26)
	for i := uint64(0); i < count; i++ {
		f.Read(row)
		if int(binary.LittleEndian.Uint16(row[0:])) == targetDay {
			return binary.LittleEndian.Uint64(row[2:]), binary.LittleEndian.Uint64(row[10:])
		}
	}
	return 0, 0
}

// --- The Core Alpha Math (Optimized) ---

func processKernel(data []byte, startRow, endRow, warmup int) []byte {
	if startRow >= endRow {
		return nil
	}
	actualStart := startRow - warmup
	if actualStart < 0 {
		actualStart = 0
	}

	// Output buffer: Ts(8) + Px(8) + Sig(8) = 24 bytes/row
	out := make([]byte, (endRow-startRow)*FeatureSize)
	outPos := 0

	// -- State Variables --
	stateGod, stateFrag := 0.0, 0.0
	stateCntEma, stateLamEma := 0.0, 0.0
	lamProp, cumDx, cumFlow := 0.00005, 0.0, 0.0
	tradeCtr, volClock := 0, 0.0

	// -- Normalizers (The Fix) --
	zGod := &OnlineZ{}
	zFrag := &OnlineZ{}
	zProp := &OnlineZ{}
	zSurge := &OnlineZ{}
	zLam := &OnlineZ{}

	// Ring buffers for Propagator
	bufS, bufQ := make([]float64, 2048), make([]float64, 2048)
	head := 0

	prevTs, prevPx := int64(0), 0.0
	invPx, invQt := 1.0/PxScale, 1.0/QtScale

	offset := actualStart * RowSize
	limit := endRow * RowSize
	idx := actualStart

	for offset < limit {
		// 1. Zero-Copy Parse
		pxRaw := binary.LittleEndian.Uint64(data[offset+8:])
		qtyRaw := binary.LittleEndian.Uint64(data[offset+16:])
		cnt := binary.LittleEndian.Uint32(data[offset+32:])
		flags := binary.LittleEndian.Uint16(data[offset+36:])
		ts := int64(binary.LittleEndian.Uint64(data[offset+38:]))
		offset += RowSize

		px := float64(pxRaw) * invPx
		qty := float64(qtyRaw) * invQt
		cIdx := int(cnt)
		if cIdx < 1 {
			cIdx = 1
		} else if cIdx > 255 {
			cIdx = 255
		}

		sign := 1.0
		if (flags & 1) != 0 {
			sign = -1.0
		}

		dt := 0.0
		if prevTs > 0 && ts > prevTs {
			dt = float64(ts - prevTs)
		}
		ret := 0.0
		if prevPx > 0 {
			ret = math.Log(px / prevPx)
		}
		prevTs, prevPx = ts, px

		// --- Signal 1: God (Momentum) ---
		volClock += qty
		if volClock > 1.0 {
			stateGod *= 0.5
			stateFrag *= 0.5
			volClock = 0.0
		}
		gfIn := sign * qty * CountPowLook[cIdx]
		stateGod = (stateGod * math.Exp(-0.0008*dt)) + gfIn

		// --- Signal 2: Frag (Mean Reversion) ---
		gate := 1.0 / (1.0 + 12000.0*math.Abs(ret))
		fsIn := math.Pow(float64(cIdx), 1.1) * qty * gate
		stateFrag = (stateFrag * FragDecay) + fsIn

		// --- Signal 3: Propagator (Decay) ---
		head = (head + 1) & 2047
		bufS[head], bufQ[head] = sign, qty
		kProp := 0.0
		// Unrolled or vectorized by Go compiler?
		// We trust the loop, but limit check is fixed 2000
		for k := 0; k < 2000; k++ {
			bIdx := (head - k) & 2047
			kProp += bufS[bIdx] * bufQ[bIdx] * PropResKernel[k]
		}

		// Adjust Propagator sign based on recent history (Lambda Prop)
		cumDx += math.Abs(px * ret)
		cumFlow += math.Abs(sign * qty)
		tradeCtr++
		if tradeCtr >= 4000 {
			if cumFlow > 1e-9 {
				lamProp = 0.9*lamProp + 0.1*(cumDx/cumFlow)
			}
			cumDx, cumFlow, tradeCtr = 0, 0, 0
		}
		kProp = -lamProp * kProp

		// --- Signal 4: Surge (Breakout) ---
		surge := math.Max(float64(cIdx)-stateCntEma, 0.0)
		kSurge := sign * math.Pow(qty, 0.77) * math.Pow(surge, 1.45)
		stateCntEma = (float64(cIdx) * 0.002496) + (stateCntEma * 0.997504)

		// --- Signal 5: Lambda (Liquidity Shock) ---
		denom := math.Pow(qty, 0.84)
		if denom < 1e-9 {
			denom = 1.0
		}
		imp := math.Abs(ret) / denom
		kLam := sign * qty * (imp - stateLamEma)
		stateLamEma = (imp * 0.001665) + (stateLamEma * 0.998335)

		// --- NORMALIZATION & COMBINATION ---
		// We normalize here so the weights below are stable regardless of Year/Qty
		nGod := zGod.Update(stateGod)
		nFrag := zFrag.Update(stateFrag)
		nProp := zProp.Update(kProp)
		nSurge := zSurge.Update(kSurge)
		nLam := zLam.Update(kLam)

		if idx >= startRow {
			// Linear Combination of Normalized Signals (Z-Scores)
			// God, Prop, Surge, Lam are Momentum/Continuation -> Positive Weights
			// Frag is Mean Reversion -> Negative Weight
			finalSig := 0.35*nGod + 0.25*nProp + 0.15*nSurge + 0.10*nLam - 0.30*nFrag

			// Write Binary Output
			binary.LittleEndian.PutUint64(out[outPos:], uint64(ts))
			binary.LittleEndian.PutUint64(out[outPos+8:], math.Float64bits(px))
			binary.LittleEndian.PutUint64(out[outPos+16:], math.Float64bits(finalSig))
			outPos += FeatureSize
		}
		idx++
	}
	return out
}
