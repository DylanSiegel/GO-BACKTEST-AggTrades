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
	"sync"
)

const (
	StartDay = 1
	EndDay   = 31
	Warmup   = 5000
)

var (
	PropResKernel [2048]float64
	CountPowLook  [256]float64
	FragDecay     = math.Exp(-0.09)
)

func init() {
	for k := 0; k < 2048; k++ {
		PropResKernel[k] = 1.0 / math.Pow(float64(k+12), 0.41)
	}
	for c := 0; c < 256; c++ {
		CountPowLook[c] = math.Min(math.Pow(float64(c), 0.63), 8.8)
	}
}

func runBuild() {
	fmt.Printf("--- BUILDALPHA GO 2025 | %s %d-%02d ---\n", Symbol, TargetYear, TargetMonth)

	for d := StartDay; d <= EndDay; d++ {
		res := processBuildDay(d)
		fmt.Println(res)
	}
}

func processBuildDay(day int) string {
	dir := filepath.Join(BaseDir, Symbol, fmt.Sprintf("%04d", TargetYear), fmt.Sprintf("%02d", TargetMonth))
	idxPath := filepath.Join(dir, "index.quantdev")
	dataPath := filepath.Join(dir, "data.quantdev")

	offset, length := findBlob(idxPath, day)
	if length == 0 {
		return fmt.Sprintf("MISSING %d", day)
	}

	f, err := os.Open(dataPath)
	if err != nil {
		return fmt.Sprintf("ERR_IO %d", day)
	}
	defer f.Close()
	f.Seek(int64(offset), 0)
	compData := make([]byte, length)
	f.Read(compData)

	r, err := zlib.NewReader(bytes.NewReader(compData))
	if err != nil {
		return fmt.Sprintf("ERR_ZLIB %d", day)
	}
	blob, err := io.ReadAll(r)
	r.Close()

	if len(blob) < HeaderSize {
		return "ERR_HDR"
	}

	rowCount := binary.LittleEndian.Uint64(blob[8:])
	body := blob[HeaderSize:]

	if uint64(len(body)) != rowCount*RowSize {
		return "ERR_SIZE"
	}

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

	var outBuf bytes.Buffer
	for _, res := range results {
		outBuf.Write(res)
	}

	outDir := filepath.Join(BaseDir, "features", Symbol, fmt.Sprintf("%04d", TargetYear), fmt.Sprintf("%02d", TargetMonth))
	os.MkdirAll(outDir, 0755)
	outPath := filepath.Join(outDir, fmt.Sprintf("%02d.bin", day))
	os.WriteFile(outPath, outBuf.Bytes(), 0644)

	return fmt.Sprintf("DONE %02d | %d rows", day, rowCount)
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

func processKernel(data []byte, startRow, endRow, warmup int) []byte {
	if startRow >= endRow {
		return nil
	}
	actualStart := startRow - warmup
	if actualStart < 0 {
		actualStart = 0
	}

	out := make([]byte, (endRow-startRow)*FeatureSize)
	outPos := 0

	stateGod, stateFrag := 0.0, 0.0
	stateCntEma, stateLamEma := 0.0, 0.0
	lamProp, cumDx, cumFlow := 0.00005, 0.0, 0.0
	tradeCtr, volClock := 0, 0.0

	bufS, bufQ := make([]float64, 2048), make([]float64, 2048)
	head := 0

	prevTs, prevPx := int64(0), 0.0
	invPx, invQt := 1.0/PxScale, 1.0/QtScale

	offset := actualStart * RowSize
	limit := endRow * RowSize
	idx := actualStart

	for offset < limit {
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

		volClock += qty
		if volClock > 1.0 {
			stateGod *= 0.5
			stateFrag *= 0.5
			volClock = 0.0
		}

		gfIn := sign * qty * CountPowLook[cIdx]
		stateGod = (stateGod * math.Exp(-0.0008*dt)) + gfIn

		gate := 1.0 / (1.0 + 12000.0*math.Abs(ret))
		fsIn := math.Pow(float64(cIdx), 1.1) * qty * gate
		stateFrag = (stateFrag * FragDecay) + fsIn

		head = (head + 1) & 2047
		bufS[head], bufQ[head] = sign, qty

		kProp := 0.0
		for k := 0; k < 2000; k++ {
			bIdx := (head - k) & 2047
			kProp += bufS[bIdx] * bufQ[bIdx] * PropResKernel[k]
		}

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

		surge := math.Max(float64(cIdx)-stateCntEma, 0.0)
		kSurge := sign * math.Pow(qty, 0.77) * math.Pow(surge, 1.45)
		stateCntEma = (float64(cIdx) * 0.002496) + (stateCntEma * 0.997504)

		denom := math.Pow(qty, 0.84)
		if denom < 1e-9 {
			denom = 1.0
		}
		imp := math.Abs(ret) / denom
		kLam := sign * qty * (imp - stateLamEma)
		stateLamEma = (imp * 0.001665) + (stateLamEma * 0.998335)

		if idx >= startRow {
			finalSig := 0.31*stateGod + 0.26*(-stateFrag) + 0.20*kProp + 0.13*kSurge + 0.10*kLam
			binary.LittleEndian.PutUint64(out[outPos:], uint64(ts))
			binary.LittleEndian.PutUint64(out[outPos+8:], math.Float64bits(px))
			binary.LittleEndian.PutUint64(out[outPos+16:], math.Float64bits(finalSig))
			outPos += FeatureSize
		}
		idx++
	}
	return out
}
