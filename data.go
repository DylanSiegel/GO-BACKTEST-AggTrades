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

// --- Local Constants (Specific to Data Downloader) ---
const (
	// Data Source
	HostData   = "data.binance.vision"
	S3Prefix   = "data/futures/um"
	DataSet    = "aggTrades"
	FallbackDt = "2020-01-01"
)

// --- Globals ---
var (
	httpClient *http.Client
	stopEvent  bool
	stopMu     sync.Mutex

	// Directory Locks to fix race conditions on monthly files
	dirLocks sync.Map
)

func init() {
	// High-throughput Transport
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}
	httpClient = &http.Client{
		Transport: tr,
		Timeout:   20 * time.Second,
	}
}

// runData is called by shared.go -> main()
func runData() {
	// Graceful Shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		stopMu.Lock()
		stopEvent = true
		stopMu.Unlock()
		fmt.Println("\n[warn] Stopping gracefully (finish current jobs)...")
	}()

	fmt.Printf("--- data.go (Ryzen 7900X Optimized) | Symbol: %s ---\n", Symbol)

	start, _ := time.Parse("2006-01-02", FallbackDt)
	end := time.Now().UTC().AddDate(0, 0, -1)

	if end.Before(start) {
		fmt.Println("Nothing to do.")
		return
	}

	// Generate Job Queue
	var days []time.Time
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		days = append(days, d)
	}

	fmt.Printf("[job] Processing %d days using %d threads.\n", len(days), CPUThreads)

	jobs := make(chan time.Time, len(days))
	results := make(chan string, len(days))
	var wg sync.WaitGroup

	// Workers
	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				// Check Stop Signal
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

	// Stats
	stats := make(map[string]int)
	for r := range results {
		key := strings.Split(r, " ")[0]
		stats[key]++
	}
	fmt.Printf("\n[done] %v\n", stats)
}

func processDay(d time.Time) string {
	y, m, day := d.Year(), int(d.Month()), d.Day()

	// Paths
	dirPath := filepath.Join(BaseDir, Symbol, fmt.Sprintf("%04d", y), fmt.Sprintf("%02d", m))
	idxPath := filepath.Join(dirPath, "index.quantdev")
	dataPath := filepath.Join(dirPath, "data.quantdev")

	// 1. Get Directory Lock (Crucial Fix for IO Race)
	// We allow concurrent DL/Parse, but serialize Index/Data checks & writes per month.
	muAny, _ := dirLocks.LoadOrStore(dirPath, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)

	// 2. Check Index (Fast Read)
	mu.Lock()
	indexed := isIndexed(idxPath, day)
	mu.Unlock()

	if indexed {
		return "skip"
	}

	// 3. Download (Concurrent / Slow IO)
	url := fmt.Sprintf("https://%s/%s/daily/%s/%s/%s-%s-%04d-%02d-%02d.zip",
		HostData, S3Prefix, DataSet, Symbol, Symbol, DataSet, y, m, day)

	zipBytes, err := download(url)
	if err != nil {
		if err == errNotFound {
			return "missing"
		}
		return "error_dl"
	}

	// 4. Fast Parse (Concurrent / High CPU)
	aggBlob, count, err := fastZipToAgg3(d, zipBytes)
	if err != nil {
		return "error_parse"
	}
	if count == 0 {
		return "empty"
	}

	// 5. Compress (Concurrent / High CPU)
	var b bytes.Buffer
	w, _ := zlib.NewWriterLevel(&b, zlib.BestSpeed) // Level 1 is fastest, sufficient for numbers
	w.Write(aggBlob)
	w.Close()
	compBlob := b.Bytes()

	// 6. Checksum (Concurrent)
	sum := sha256.Sum256(aggBlob)
	cSum := binary.LittleEndian.Uint64(sum[:8])

	// 7. Write Data & Update Index (Serialized / Fast IO)
	mu.Lock()
	defer mu.Unlock()

	// Double check index in case another thread finished this day while we were processing
	if isIndexed(idxPath, day) {
		return "skip_race"
	}

	if err := os.MkdirAll(dirPath, 0755); err != nil {
		return "error_mkdir"
	}

	// Append Data
	fData, err := os.OpenFile(dataPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return "error_io"
	}

	stat, _ := fData.Stat()
	offset := stat.Size()

	if _, err := fData.Write(compBlob); err != nil {
		fData.Close()
		return "error_write"
	}
	fData.Close() // Close immediately to flush

	// Append Index
	fIdx, err := os.OpenFile(idxPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return "error_idx"
	}
	defer fIdx.Close()

	idxStat, _ := fIdx.Stat()
	if idxStat.Size() == 0 {
		// Init Index Header
		hdr := make([]byte, 16)
		copy(hdr[0:], IdxMagic)
		binary.LittleEndian.PutUint32(hdr[4:], uint32(IdxVersion))
		binary.LittleEndian.PutUint64(hdr[8:], 0) // Count = 0
		fIdx.Write(hdr)
	}

	// Index Row: Day(2), Offset(8), Length(8), Checksum(8) = 26 bytes
	row := make([]byte, 26)
	binary.LittleEndian.PutUint16(row[0:], uint16(day))
	binary.LittleEndian.PutUint64(row[2:], uint64(offset))
	binary.LittleEndian.PutUint64(row[10:], uint64(len(compBlob)))
	binary.LittleEndian.PutUint64(row[18:], cSum)

	if _, err := fIdx.Seek(0, io.SeekEnd); err != nil {
		return "error_idx_seek"
	}
	if _, err := fIdx.Write(row); err != nil {
		return "error_idx_write"
	}

	// Increment Index Count (Atomic update under lock)
	fIdx.Seek(8, io.SeekStart)
	var currentCount uint64
	binary.Read(fIdx, binary.LittleEndian, &currentCount)
	fIdx.Seek(8, io.SeekStart)
	binary.Write(fIdx, binary.LittleEndian, currentCount+1)

	return "ok"
}

// --- Helpers ---

var errNotFound = fmt.Errorf("404")

func download(url string) ([]byte, error) {
	// Retry logic optimized for throughput
	for i := 0; i < 3; i++ {
		resp, err := httpClient.Get(url)
		if err == nil {
			if resp.StatusCode == 200 {
				data, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				return data, err
			}
			resp.Body.Close()
			if resp.StatusCode == 404 {
				return nil, errNotFound
			}
		}
		time.Sleep(time.Duration(i+1) * 100 * time.Millisecond)
	}
	return nil, fmt.Errorf("timeout")
}

func isIndexed(idxPath string, day int) bool {
	f, err := os.Open(idxPath)
	if err != nil {
		return false
	}
	defer f.Close()

	// Read Header
	hdr := make([]byte, 16)
	if _, err := f.Read(hdr); err != nil {
		return false
	}
	if string(hdr[:4]) != IdxMagic {
		return false
	}
	count := binary.LittleEndian.Uint64(hdr[8:])

	// Scan Rows
	row := make([]byte, 26)
	// Optimization: If count is huge, this is slow, but for monthly files (max 31 rows), it's instant.
	for i := uint64(0); i < count; i++ {
		if _, err := f.Read(row); err != nil {
			break
		}
		if int(binary.LittleEndian.Uint16(row[0:])) == day {
			return true
		}
	}
	return false
}

// fastZipToAgg3: Zero-alloc column scanning + Binary Packing
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

		// 32GB RAM allows reading full CSV.
		// For max perf, we read all at once to avoid small IO/syscall overhead.
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, 0, err
		}

		// Estimation: ~70 bytes CSV -> 48 bytes Bin
		estRows := len(data) / 50
		blob := make([]byte, 0, estRows*RowSize)
		rowBuf := make([]byte, RowSize)

		var (
			minTs int64 = math.MaxInt64
			maxTs int64 = math.MinInt64
			count uint64

			// Column Tracking
			// 0:id, 1:px, 2:qty, 3:fid, 4:lid, 5:ts, 6:m
			colIdx int
			start  int
			i      int
			n      = len(data)
		)

		// Skip Header Logic
		for i < n {
			if data[i] == '\n' {
				i++
				start = i
				break
			}
			i++
		}

		// State Machine Loop
		for i < n {
			b := data[i]

			// Fixed: QF1003 tagged switch on b
			switch b {
			case ',':
				// We only parse if we need the column content.
				// But we parse all for validation/packing.
				// Slice is zero-copy (view into 'data')
				colSlice := data[start:i]

				switch colIdx {
				case 0: // AggID
					binary.LittleEndian.PutUint64(rowBuf[0:], fastParseUint(colSlice))
				case 1: // Price
					binary.LittleEndian.PutUint64(rowBuf[8:], fastParseFloatFixed(colSlice))
				case 2: // Qty
					binary.LittleEndian.PutUint64(rowBuf[16:], fastParseFloatFixed(colSlice))
				case 3: // FirstID
					binary.LittleEndian.PutUint64(rowBuf[24:], fastParseUint(colSlice))
				case 4: // LastID -> Count
					// Need FirstID to calc count. FirstID is already in rowBuf[24:]
					fid := binary.LittleEndian.Uint64(rowBuf[24:])
					lid := fastParseUint(colSlice)
					// Count = Last - First + 1
					binary.LittleEndian.PutUint32(rowBuf[32:], uint32(lid-fid+1))
				case 5: // Time
					ts := fastParseUint(colSlice)
					binary.LittleEndian.PutUint64(rowBuf[38:], ts)
					if int64(ts) < minTs {
						minTs = int64(ts)
					}
					if int64(ts) > maxTs {
						maxTs = int64(ts)
					}
				}

				colIdx++
				start = i + 1

			case '\n':
				// End of row (Maker Flag)
				colSlice := data[start:i]

				// Case 6: is_buyer_maker
				// Check 't' or 'T' (optimized check)
				flags := uint16(0)
				if len(colSlice) > 0 {
					c := colSlice[0]
					if c == 't' || c == 'T' {
						flags = 1
					}
				}
				binary.LittleEndian.PutUint16(rowBuf[36:], flags)

				// Append Row
				blob = append(blob, rowBuf...)
				count++

				colIdx = 0
				start = i + 1
			}
			i++
		}

		// Handle case where file doesn't end with \n
		if colIdx == 6 && start < n {
			colSlice := data[start:n]
			flags := uint16(0)
			if len(colSlice) > 0 && (colSlice[0] == 't' || colSlice[0] == 'T') {
				flags = 1
			}
			binary.LittleEndian.PutUint16(rowBuf[36:], flags)
			blob = append(blob, rowBuf...)
			count++
		}

		if count == 0 {
			return nil, 0, nil
		}

		// Header construction (Matches shared.go AggHeader struct: 48 bytes)
		hdr := make([]byte, HeaderSize) // HeaderSize is 48 from shared.go
		copy(hdr[0:], AggMagic)
		hdr[4] = 1
		hdr[5] = uint8(t.Day())
		binary.LittleEndian.PutUint16(hdr[6:], 3) // zlib level (informational)
		binary.LittleEndian.PutUint64(hdr[8:], count)
		binary.LittleEndian.PutUint64(hdr[16:], uint64(minTs))
		binary.LittleEndian.PutUint64(hdr[24:], uint64(maxTs))
		// Bytes 32-47 are padding (zero initialized by make)

		return append(hdr, blob...), count, nil
	}
	return nil, 0, fmt.Errorf("no csv")
}

// --- High Performance Parsers (No Alloc) ---

func fastParseUint(b []byte) uint64 {
	var n uint64
	for _, c := range b {
		// Rely on valid ASCII digits 0-9
		n = n*10 + uint64(c-'0')
	}
	return n
}

// Converts "123.45" -> 12345000000 (scaled 1e8)
func fastParseFloatFixed(b []byte) uint64 {
	var n uint64
	seenDot := false
	decimals := 0

	for _, c := range b {
		if c == '.' {
			seenDot = true
			continue
		}
		// Branchless logic possible, but this is fast enough for memory-bound tasks
		n = n*10 + uint64(c-'0')
		if seenDot {
			decimals++
		}
	}

	// Adjust Scale 1e8
	const target = 8
	if decimals < target {
		// Hand-rolled power of 10 for small N is faster than math.Pow
		for i := 0; i < (target - decimals); i++ {
			n *= 10
		}
	} else if decimals > target {
		for i := 0; i < (decimals - target); i++ {
			n /= 10
		}
	}
	return n
}
