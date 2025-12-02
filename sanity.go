package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// runSanity scans all months for the currently active Symbol() and validates
// that index.quantdev and data.quantdev are consistent, including:
//   - presence of both files
//   - index header magic and row count
//   - compressed blobs readable and checksummed
//   - AGG3 header present and valid
//   - AGG3 body length matches HeaderSize + rowCount*RowSize
func runSanity() {
	root := filepath.Join(BaseDir, Symbol())
	dirs, err := os.ReadDir(root)
	if err != nil {
		fmt.Printf("SANITY: cannot read root %s: %v\n", root, err)
		return
	}

	var tasks []string
	for _, y := range dirs {
		if !y.IsDir() {
			continue
		}
		months, err := os.ReadDir(filepath.Join(root, y.Name()))
		if err != nil {
			fmt.Printf("SANITY: cannot read year %s: %v\n", y.Name(), err)
			continue
		}
		for _, m := range months {
			if !m.IsDir() {
				continue
			}
			tasks = append(tasks, filepath.Join(root, y.Name(), m.Name()))
		}
	}

	fmt.Printf("SANITY CHECK: %s (%d months)\n", Symbol(), len(tasks))
	if len(tasks) == 0 {
		return
	}

	var wg sync.WaitGroup
	jobs := make(chan string, len(tasks))

	for i := 0; i < CPUThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				validateMonth(path)
			}
		}()
	}

	for _, t := range tasks {
		jobs <- t
	}
	close(jobs)
	wg.Wait()
}

func validateMonth(dir string) {
	idxPath := filepath.Join(dir, "index.quantdev")
	dataPath := filepath.Join(dir, "data.quantdev")

	fIdx, err := os.Open(idxPath)
	if err != nil {
		fmt.Printf("FAIL: %s (No Index: %v)\n", dir, err)
		return
	}
	defer fIdx.Close()

	fData, err := os.Open(dataPath)
	if err != nil {
		fmt.Printf("FAIL: %s (No Data: %v)\n", dir, err)
		return
	}
	defer fData.Close()

	var hdr [16]byte
	if _, err := io.ReadFull(fIdx, hdr[:]); err != nil {
		fmt.Printf("FAIL: %s (Header Read Error: %v)\n", dir, err)
		return
	}
	if string(hdr[:4]) != IdxMagic {
		fmt.Printf("FAIL: %s (Bad Index Magic)\n", dir)
		return
	}

	count := binary.LittleEndian.Uint64(hdr[8:])
	var row [26]byte

	issues := 0
	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(fIdx, row[:]); err != nil {
			issues++
			break
		}
		offset := int64(binary.LittleEndian.Uint64(row[2:]))
		length := int(binary.LittleEndian.Uint64(row[10:]))
		expSum := binary.LittleEndian.Uint64(row[18:])

		if length <= 0 {
			issues++
			continue
		}

		if _, err := fData.Seek(offset, io.SeekStart); err != nil {
			issues++
			continue
		}

		compData := make([]byte, length)
		if _, err := io.ReadFull(fData, compData); err != nil {
			issues++
			continue
		}

		r, err := zlib.NewReader(bytes.NewReader(compData))
		if err != nil {
			issues++
			continue
		}
		aggBlob, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			issues++
			continue
		}

		// Check checksum of full AGG3 blob.
		s := sha256.Sum256(aggBlob)
		if binary.LittleEndian.Uint64(s[:8]) != expSum {
			issues++
			continue
		}

		// Validate AGG3 header.
		if len(aggBlob) < HeaderSize {
			issues++
			continue
		}
		if string(aggBlob[:4]) != AggMagic {
			issues++
			continue
		}

		// Optional but stronger check: rowCount vs length.
		rowCount := binary.LittleEndian.Uint64(aggBlob[8:16])
		expectedSize := HeaderSize + int(rowCount)*RowSize
		if expectedSize != len(aggBlob) {
			issues++
			continue
		}
	}

	if issues > 0 {
		fmt.Printf("ISSUES: %s (%d errors)\n", dir, issues)
	}
}
