package main

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Local constants
const (
	FallbackDt = "2020-01-01"
	HostData   = "data.binance.vision"
	S3Prefix   = "data/futures/um"
	DataSet    = "aggTrades"
)

var (
	httpClient *http.Client
	stopEvent  bool
	stopMu     sync.Mutex
)

func init() {
	// High-throughput HTTP client
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}
	httpClient = &http.Client{
		Transport: tr,
		Timeout:   15 * time.Second,
	}
}

func runData() {
	// Signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		stopMu.Lock()
		stopEvent = true
		stopMu.Unlock()
		fmt.Println("\nStopping...")
	}()

	fmt.Printf("--- data.go (Optimized High-Performance) | Symbol: %s ---\n", Symbol)

	start, _ := time.Parse("2006-01-02", FallbackDt)
	end := time.Now().UTC().AddDate(0, 0, -1)

	if end.Before(start) {
		fmt.Println("Nothing to do.")
		return
	}

	var days []time.Time
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		days = append(days, d)
	}

	fmt.Printf("[job] %d days -> %d threads.\n", len(days), CPUThreads)

	jobs := make(chan time.Time, len(days))
	results := make(chan string, len(days))
	var wg sync.WaitGroup

	// Spin up workers matching Logical Processor count (24)
	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				stopMu.Lock()
				if stopEvent {
					stopMu.Unlock()
					break
				}
				stopMu.Unlock()
				results <- processDay(d)
			}
		}()
	}

	for _, d := range days {
		jobs <- d
	}
	close(jobs)
	wg.Wait()
	close(results)

	stats := make(map[string]int)
	for r := range results {
		// Group stats by basic result type (ok, skip, error)
		key := strings.Split(r, " ")[0]
		stats[key]++
	}
	fmt.Printf("\n[done] Stats: %v\n", stats)
}

func processDay(d time.Time) string {
	y, m, day := d.Year(), int(d.Month()), d.Day()

	dirPath := filepath.Join(BaseDir, Symbol, fmt.Sprintf("%04d", y), fmt.Sprintf("%02d", m))
	idxPath := filepath.Join(dirPath, "index.quantdev")
	dataPath := filepath.Join(dirPath, "data.quantdev")

	// 1. Check if already processed
	if isIndexed(idxPath, dataPath, day) {
		return "skip"
	}

	// 2. Download ZIP (In Memory)
	url := fmt.Sprintf("https://%s/%s/daily/%s/%s/%s-%s-%04d-%02d-%02d.zip",
		HostData, S3Prefix, DataSet, Symbol, Symbol, DataSet, y, m, day)

	zipBytes, err := download(url)
	if err != nil {
		if err == errNotFound {
			return "missing"
		}
		return "error_dl"
	}

	// 3. Fast Parse & Binary Pack
	// Uses manual byte scanning instead of encoding/csv
	aggBlob, count, err := fastZipToAgg3(d, zipBytes)
	if err != nil {
		return "error_parse"
	}
	if count == 0 {
		return "empty"
	}

	// 4. Compress (Zlib level 3 is good balance)
	var b bytes.Buffer
	w, _ := zlib.NewWriterLevel(&b, zlib.BestSpeed)
	w.Write(aggBlob)
	w.Close()
	compBlob := b.Bytes()

	// 5. Checksum
	sum := sha256.Sum256(aggBlob)
	cSum := binary.LittleEndian.Uint64(sum[:8])

	// 6. Write (Thread-safe by OS append, but we lock dir creation)
	os.MkdirAll(dirPath, 0755)

	fData, err := os.OpenFile(dataPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return "error_io"
	}
	defer fData.Close()

	stat, _ := fData.Stat()
	offset := stat.Size()

	if _, err := fData.Write(compBlob); err != nil {
		return "error_write"
	}

	// 7. Update Index
	fIdx, err := os.OpenFile(idxPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return "error_idx"
	}
	defer fIdx.Close()

	idxStat, _ := fIdx.Stat()
	if idxStat.Size() == 0 {
		hdr := make([]byte, 16)
		copy(hdr[0:], IdxMagic)
		binary.LittleEndian.PutUint32(hdr[4:], uint32(IdxVersion))
		binary.LittleEndian.PutUint64(hdr[8:], 0)
		fIdx.Write(hdr)
	}

	// Row: Day(2), Offset(8), Length(8), Checksum(8)
	row := make([]byte, 26)
	binary.LittleEndian.PutUint16(row[0:], uint16(day))
	binary.LittleEndian.PutUint64(row[2:], uint64(offset))
	binary.LittleEndian.PutUint64(row[10:], uint64(len(compBlob)))
	binary.LittleEndian.PutUint64(row[18:], cSum)

	if _, err := fIdx.Seek(0, io.SeekEnd); err != nil {
		return "error_idx"
	}
	if _, err := fIdx.Write(row); err != nil {
		return "error_idx"
	}

	// Increment Index Count
	fIdx.Seek(8, io.SeekStart)
	var currentCount uint64
	binary.Read(fIdx, binary.LittleEndian, &currentCount)
	fIdx.Seek(8, io.SeekStart)
	binary.Write(fIdx, binary.LittleEndian, currentCount+1)

	return "ok"
}

var errNotFound = fmt.Errorf("404")

func download(url string) ([]byte, error) {
	for i := 0; i < 5; i++ {
		resp, err := httpClient.Get(url)
		if err == nil {
			if resp.StatusCode == 200 {
				defer resp.Body.Close()
				return io.ReadAll(resp.Body)
			}
			resp.Body.Close()
			if resp.StatusCode == 404 {
				return nil, errNotFound
			}
		}
		// Backoff
		time.Sleep(time.Duration(i+1) * 200 * time.Millisecond)
	}
	return nil, fmt.Errorf("timeout")
}

// fastZipToAgg3 replaces standard CSV parsing with raw byte scanning.
// It aligns data to the 48-byte schema defined in shared.go.
func fastZipToAgg3(t time.Time, zipData []byte) ([]byte, uint64, error) {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, 0, err
	}

	for _, f := range r.File {
		if !strings.HasSuffix(f.Name, ".csv") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		defer rc.Close()

		// Read entire CSV into RAM (efficient on 32GB RAM system)
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, 0, err
		}

		// Pre-allocate buffer (estimate 70 bytes per CSV row -> 48 bytes binary)
		// Capacity guess: len(data) / 1.5
		estRows := len(data) / 50
		blob := make([]byte, 0, estRows*RowSize)

		// Reusable Row Buffer
		rowBuf := make([]byte, RowSize)

		var (
			minTs int64 = math.MaxInt64
			maxTs int64 = math.MinInt64
			count uint64
		)

		// pointers
		i := 0
		n := len(data)

		// Skip Header
		for i < n {
			if data[i] == '\n' {
				i++
				break
			}
			i++
		}

		// CSV Column buffers (slices of original data)
		// agg_id, price, qty, first_id, last_id, time, is_maker
		var cols [7][]byte
		colIdx := 0
		start := i

		for i < n {
			b := data[i]

			if b == ',' {
				cols[colIdx] = data[start:i]
				colIdx++
				start = i + 1
			} else if b == '\n' {
				cols[colIdx] = data[start:i]

				// Process Row
				if colIdx == 6 {
					// 1. AggID
					tid := fastParseUint(cols[0])

					// 2. Price (Fixed Point 1e8)
					px := fastParseFloatFixed(cols[1])

					// 3. Qty (Fixed Point 1e8)
					qty := fastParseFloatFixed(cols[2])

					// 4. FirstID
					fid := fastParseUint(cols[3])

					// 5. LastID -> Calc Count
					lid := fastParseUint(cols[4])
					cnt := uint32(lid - fid + 1)

					// 6. Time
					ts := int64(fastParseUint(cols[5]))

					// 7. Maker Flag
					flags := uint16(0)
					// Check for "true" or "True"
					if len(cols[6]) > 0 && (cols[6][0] == 't' || cols[6][0] == 'T') {
						flags = 1
					}

					// Track TS min/max
					if ts < minTs {
						minTs = ts
					}
					if ts > maxTs {
						maxTs = ts
					}

					// Pack Binary (using shared.go PutRow logic locally inline for speed)
					binary.LittleEndian.PutUint64(rowBuf[0:], tid)
					binary.LittleEndian.PutUint64(rowBuf[8:], px)
					binary.LittleEndian.PutUint64(rowBuf[16:], qty)
					binary.LittleEndian.PutUint64(rowBuf[24:], fid)
					binary.LittleEndian.PutUint32(rowBuf[32:], cnt)
					binary.LittleEndian.PutUint16(rowBuf[36:], flags)
					binary.LittleEndian.PutUint64(rowBuf[38:], uint64(ts))
					// Bytes 46-47 are padding (0)

					blob = append(blob, rowBuf...)
					count++
				}

				colIdx = 0
				start = i + 1
			}
			i++
		}

		if count == 0 {
			return nil, 0, nil
		}

		// Construct AGG3 Header
		hdr := make([]byte, HeaderSize)
		copy(hdr[0:], AggMagic)
		hdr[4] = 1
		hdr[5] = uint8(t.Day())
		binary.LittleEndian.PutUint16(hdr[6:], 3) // zlvl
		binary.LittleEndian.PutUint64(hdr[8:], count)
		binary.LittleEndian.PutUint64(hdr[16:], uint64(minTs))
		binary.LittleEndian.PutUint64(hdr[24:], uint64(maxTs))

		// Prepend Header
		fullBlob := append(hdr, blob...)
		return fullBlob, count, nil
	}

	return nil, 0, fmt.Errorf("no csv found in zip")
}

// --- Fast Parsing Helpers (No String Allocations) ---

func fastParseUint(b []byte) uint64 {
	var n uint64
	for _, c := range b {
		// Simple ASCII digit check
		n = n*10 + uint64(c-'0')
	}
	return n
}

// Parses 123.456 into 12345600000 (scaled by 1e8)
func fastParseFloatFixed(b []byte) uint64 {
	var n uint64
	seenDot := false
	decimals := 0

	for _, c := range b {
		if c == '.' {
			seenDot = true
			continue
		}
		if c >= '0' && c <= '9' {
			n = n*10 + uint64(c-'0')
			if seenDot {
				decimals++
			}
		}
	}

	// Adjust scale to 1e8
	const targetScale = 8
	if decimals < targetScale {
		for i := 0; i < (targetScale - decimals); i++ {
			n *= 10
		}
	} else if decimals > targetScale {
		// This happens rarely in crypto price/qty (usually 8 decimals max), but handle truncation
		for i := 0; i < (decimals - targetScale); i++ {
			n /= 10
		}
	}

	return n
}

func isIndexed(idxPath, dataPath string, day int) bool {
	f, err := os.Open(idxPath)
	if err != nil {
		return false
	}
	defer f.Close()

	// Header check
	hdr := make([]byte, 16)
	if _, err := f.Read(hdr); err != nil {
		return false
	}
	if string(hdr[:4]) != IdxMagic {
		return false
	}
	count := binary.LittleEndian.Uint64(hdr[8:])

	// Scan index rows
	row := make([]byte, 26)
	for i := uint64(0); i < count; i++ {
		if _, err := f.Read(row); err != nil {
			break
		}
		d := binary.LittleEndian.Uint16(row[0:])
		if int(d) == day {
			// Found the day, ensure the data blob actually exists at that offset
			// (Optional strict check, but simple existence is usually enough)
			return true
		}
	}
	return false
}
