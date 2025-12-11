package main

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"crypto/sha256"
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
	"sync/atomic"
	"time"
)

const (
	HostData   = "data.binance.vision"
	S3Prefix   = "data/futures/um"
	DataSet    = "aggTrades"
	FallbackDt = "2020-01-01"
)

var (
	httpClient *http.Client
	stopEvent  atomic.Bool
	dirLocks   sync.Map
)

func init() {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}
	httpClient = &http.Client{
		Transport: tr,
		Timeout:   20 * time.Second,
	}
}

func runData() {
	sigChan := make(chan os.Signal, 1)
	// On Windows, os.Interrupt is the relevant signal.
	signal.Notify(sigChan, os.Interrupt)
	go func() {
		<-sigChan
		stopEvent.Store(true)
		fmt.Println("\n[warn] Stopping gracefully (finish current jobs)...")
	}()

	fmt.Printf("--- data.go (Apple Silicon Optimized) | Symbol: %s ---\n", Symbol())

	start, err := time.Parse("2006-01-02", FallbackDt)
	if err != nil {
		fmt.Printf("[fatal] invalid FallbackDt %q: %v\n", FallbackDt, err)
		return
	}

	end := time.Now().UTC().AddDate(0, 0, -1)
	if end.Before(start) {
		fmt.Println("Nothing to do.")
		return
	}

	var days []time.Time
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		days = append(days, d)
	}

	fmt.Printf("[job] Processing %d days using %d threads (M-Series optimized).\n", len(days), CPUThreads)

	jobs := make(chan time.Time, len(days))
	results := make(chan string, len(days))
	var wg sync.WaitGroup

	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				if stopEvent.Load() {
					return
				}
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
		key := strings.SplitN(r, " ", 2)[0]
		stats[key]++
	}
	fmt.Printf("\n[done] %v\n", stats)
}

func processDay(d time.Time) string {
	y, m, day := d.Year(), int(d.Month()), d.Day()

	dirPath := filepath.Join(BaseDir, Symbol(), fmt.Sprintf("%04d", y), fmt.Sprintf("%02d", m))
	idxPath := filepath.Join(dirPath, "index.quantdev")
	dataPath := filepath.Join(dirPath, "data.quantdev")

	muAny, _ := dirLocks.LoadOrStore(dirPath, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)

	mu.Lock()
	indexed := isIndexed(idxPath, day)
	mu.Unlock()

	if indexed {
		return "skip"
	}

	sym := Symbol()
	url := fmt.Sprintf("https://%s/%s/daily/%s/%s/%s-%s-%04d-%02d-%02d.zip",
		HostData, S3Prefix, DataSet, sym, sym, DataSet, y, m, day)

	zipBytes, err := download(url)
	if err != nil {
		if err == errNotFound {
			return "missing"
		}
		return "error_dl"
	}

	aggBlob, count, err := fastZipToAgg3(d, zipBytes)
	if err != nil {
		return "error_parse"
	}
	if count == 0 {
		return "empty"
	}

	// Compression uses zlib.BestSpeed, balancing CPU/IO on fast NVMe drives.
	var b bytes.Buffer
	w, err := zlib.NewWriterLevel(&b, zlib.BestSpeed)
	if err != nil {
		return "error_zlib"
	}
	if _, err := w.Write(aggBlob); err != nil {
		w.Close()
		return "error_zlib_write"
	}
	if err := w.Close(); err != nil {
		return "error_zlib_close"
	}
	compBlob := b.Bytes()

	sum := sha256.Sum256(aggBlob)
	cSum := binary.LittleEndian.Uint64(sum[:8])

	mu.Lock()
	defer mu.Unlock()

	if isIndexed(idxPath, day) {
		return "skip_race"
	}

	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		return "error_mkdir"
	}

	fData, err := os.OpenFile(dataPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "error_io"
	}
	stat, err := fData.Stat()
	if err != nil {
		fData.Close()
		return "error_stat"
	}
	offset := stat.Size()

	if _, err := fData.Write(compBlob); err != nil {
		fData.Close()
		return "error_write"
	}
	fData.Close()

	fIdx, err := os.OpenFile(idxPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return "error_idx"
	}
	defer fIdx.Close()

	idxStat, err := fIdx.Stat()
	if err != nil {
		return "error_idx_stat"
	}
	if idxStat.Size() == 0 {
		var hdr [16]byte
		copy(hdr[0:], IdxMagic)
		binary.LittleEndian.PutUint32(hdr[4:], uint32(IdxVersion))
		binary.LittleEndian.PutUint64(hdr[8:], 0)
		if _, err := fIdx.Write(hdr[:]); err != nil {
			return "error_idx_hdr"
		}
	}

	var row [26]byte
	binary.LittleEndian.PutUint16(row[0:], uint16(day))
	binary.LittleEndian.PutUint64(row[2:], uint64(offset))
	binary.LittleEndian.PutUint64(row[10:], uint64(len(compBlob)))
	binary.LittleEndian.PutUint64(row[18:], cSum)

	if _, err := fIdx.Seek(0, io.SeekEnd); err != nil {
		return "error_idx_seek"
	}
	if _, err := fIdx.Write(row[:]); err != nil {
		return "error_idx_write"
	}

	if _, err := fIdx.Seek(8, io.SeekStart); err != nil {
		return "error_idx_seek"
	}
	var currentCount uint64
	if err := binary.Read(fIdx, binary.LittleEndian, &currentCount); err != nil {
		return "error_idx_read"
	}
	if _, err := fIdx.Seek(8, io.SeekStart); err != nil {
		return "error_idx_seek"
	}
	if err := binary.Write(fIdx, binary.LittleEndian, currentCount+1); err != nil {
		return "error_idx_write"
	}

	return "ok"
}

// --- Helpers ---

var errNotFound = fmt.Errorf("404")

func download(url string) ([]byte, error) {
	for i := 0; i < 3; i++ {
		resp, err := httpClient.Get(url)
		if err == nil {
			if resp != nil {
				if resp.StatusCode == http.StatusOK {
					data, readErr := io.ReadAll(resp.Body)
					resp.Body.Close()
					return data, readErr
				}
				if resp.StatusCode == http.StatusNotFound {
					resp.Body.Close()
					return nil, errNotFound
				}
				resp.Body.Close()
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

	var hdr [16]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return false
	}
	if string(hdr[:4]) != IdxMagic {
		return false
	}
	count := binary.LittleEndian.Uint64(hdr[8:])

	var row [26]byte
	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(f, row[:]); err != nil {
			break
		}
		if int(binary.LittleEndian.Uint16(row[0:])) == day {
			return true
		}
	}
	return false
}

// parseBuyerMakerFlag interprets the is_buyer_maker CSV column.
// Expect values like "true"/"false" (case-insensitive).
// Returns bit 0 = 1 if is_buyer_maker == true.
func parseBuyerMakerFlag(col []byte) uint16 {
	if len(col) == 0 {
		return 0
	}
	switch col[0] {
	case 't', 'T': // "true"
		return 1
	default:
		return 0
	}
}

// --- CSV Parser ---
//
// CSV Layout (7 columns):
//
//	0: agg_trade_id      (uint64)
//	1: price             (float, fixed-point 1e-8)
//	2: quantity          (float, fixed-point 1e-8)
//	3: first_trade_id    (uint64)
//	4: last_trade_id     (uint64)
//	5: transact_time     (uint64, ms)
//	6: is_buyer_maker    ("true"/"false")
//
// Row layout (RowSize = 48):
//
//	0..7   : agg_trade_id (uint64)          [optional, not used downstream]
//	8..15  : price_fixed (uint64)
//	16..23 : qty_fixed   (uint64)
//	24..31 : first_trade_id (uint64)
//	32..35 : trade_count = last - first + 1 (uint32)
//	36..37 : flags (uint16), bit 0 = is_buyer_maker
//	38..45 : transact_time (uint64)
//	46..47 : unused / padding
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

		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, 0, err
		}

		estRows := len(data) / 50
		if estRows < 1 {
			estRows = 1
		}
		blob := make([]byte, 0, estRows*RowSize)
		var rowBuf [RowSize]byte

		var (
			minTs  int64 = math.MaxInt64
			maxTs  int64 = math.MinInt64
			count  uint64
			colIdx int
			start  int
			i      int
			n      = len(data)
		)

		// Skip header line
		for i < n {
			if data[i] == '\n' {
				i++
				start = i
				break
			}
		i++
		}

		// Main parsing loop - ARM64 handles the byte scanning efficiently
		for i < n {
			b := data[i]
			switch b {
			case ',':
				colSlice := data[start:i]
				switch colIdx {
				case 0:
					// agg_trade_id (stored but not used downstream)
					binary.LittleEndian.PutUint64(rowBuf[0:], fastParseUint(colSlice))
				case 1:
					// price
					binary.LittleEndian.PutUint64(rowBuf[8:], fastParseFloatFixed(colSlice))
				case 2:
					// quantity
					binary.LittleEndian.PutUint64(rowBuf[16:], fastParseFloatFixed(colSlice))
				case 3:
					// first_trade_id
					binary.LittleEndian.PutUint64(rowBuf[24:], fastParseUint(colSlice))
				case 4:
					// last_trade_id -> trade count
					fid := binary.LittleEndian.Uint64(rowBuf[24:])
					lid := fastParseUint(colSlice)
					if lid >= fid {
						binary.LittleEndian.PutUint32(rowBuf[32:], uint32(lid-fid+1))
					} else {
						binary.LittleEndian.PutUint32(rowBuf[32:], 0)
					}
				case 5:
					// transact_time
					ts := fastParseUint(colSlice)
					binary.LittleEndian.PutUint64(rowBuf[38:], ts)
					ts64 := int64(ts)
					if ts64 < minTs {
						minTs = ts64
					}
					if ts64 > maxTs {
						maxTs = ts64
					}
				}
				colIdx++
				start = i + 1

			case '\n':
				// Last column: is_buyer_maker
				colSlice := data[start:i]
				flags := parseBuyerMakerFlag(colSlice)
				binary.LittleEndian.PutUint16(rowBuf[36:], flags)

				blob = append(blob, rowBuf[:]...)
				count++
				colIdx = 0
				start = i + 1
			}
			i++
		}

		// Handle final line without trailing newline
		if colIdx == 6 && start < n {
			colSlice := data[start:n]
			flags := parseBuyerMakerFlag(colSlice)
			binary.LittleEndian.PutUint16(rowBuf[36:], flags)

			blob = append(blob, rowBuf[:]...)
			count++
		}

		if count == 0 {
			return nil, 0, nil
		}

		var hdr [HeaderSize]byte
		copy(hdr[0:], AggMagic)
		hdr[4] = 1              // version
		hdr[5] = uint8(t.Day()) // day-of-month
		binary.LittleEndian.PutUint16(hdr[6:], uint16(zlib.BestSpeed))
		binary.LittleEndian.PutUint64(hdr[8:], count)
		binary.LittleEndian.PutUint64(hdr[16:], uint64(minTs))
		binary.LittleEndian.PutUint64(hdr[24:], uint64(maxTs))

		out := make([]byte, 0, HeaderSize+len(blob))
		out = append(out, hdr[:]...)
		out = append(out, blob...)
		return out, count, nil
	}
	return nil, 0, fmt.Errorf("no csv")
}

func fastParseUint(b []byte) uint64 {
	var n uint64
	for _, c := range b {
		n = n*10 + uint64(c-'0')
	}
	return n
}

const targetDecimals = 8

var pow10 = [...]uint64{
	1, 10, 100, 1000, 10000, 100000, 1000000, 10000000, 100000000,
}

func fastParseFloatFixed(b []byte) uint64 {
	var n uint64
	seenDot := false
	decimals := 0

	for _, c := range b {
		if c == '.' {
			seenDot = true
			continue
		}
		n = n*10 + uint64(c-'0')
		if seenDot {
			decimals++
		}
	}

	if decimals == targetDecimals {
		return n
	}

	if decimals < targetDecimals {
		diff := targetDecimals - decimals
		if diff < len(pow10) {
			return n * pow10[diff]
		}
		for i := 0; i < diff; i++ {
			n *= 10
		}
		return n
	}

	diff := decimals - targetDecimals
	if diff < len(pow10) {
		return n / pow10[diff]
	}
	for i := 0; i < diff; i++ {
		n /= 10
	}
	return n
}
