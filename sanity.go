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

func runSanity() {
	root := filepath.Join(BaseDir, Symbol)
	dirs, _ := os.ReadDir(root)

	var tasks []string
	for _, y := range dirs {
		if y.IsDir() {
			months, _ := os.ReadDir(filepath.Join(root, y.Name()))
			for _, m := range months {
				if m.IsDir() {
					tasks = append(tasks, filepath.Join(root, y.Name(), m.Name()))
				}
			}
		}
	}

	fmt.Printf("SANITY CHECK: %s (%d months)\n", Symbol, len(tasks))

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
		fmt.Printf("FAIL: %s (No Index)\n", dir)
		return
	}
	defer fIdx.Close()

	fData, err := os.Open(dataPath)
	if err != nil {
		fmt.Printf("FAIL: %s (No Data)\n", dir)
		return
	}
	defer fData.Close()

	hdr := make([]byte, 16)
	fIdx.Read(hdr)
	count := binary.LittleEndian.Uint64(hdr[8:])
	row := make([]byte, 26)
	issues := 0

	for i := uint64(0); i < count; i++ {
		fIdx.Read(row)
		offset := int64(binary.LittleEndian.Uint64(row[2:]))
		length := int(binary.LittleEndian.Uint64(row[10:]))
		expSum := binary.LittleEndian.Uint64(row[18:])

		compData := make([]byte, length)
		fData.Seek(offset, 0)
		fData.Read(compData)

		r, err := zlib.NewReader(bytes.NewReader(compData))
		if err != nil {
			issues++
			continue
		}
		aggBlob, _ := io.ReadAll(r)
		r.Close()

		s := sha256.Sum256(aggBlob)
		if binary.LittleEndian.Uint64(s[:8]) != expSum {
			issues++
			continue
		}

		if len(aggBlob) < HeaderSize {
			issues++
			continue
		}
		if uint64(len(aggBlob)) != HeaderSize+binary.LittleEndian.Uint64(aggBlob[8:])*RowSize {
			issues++
		}
	}

	if issues > 0 {
		fmt.Printf("ISSUES: %s (%d errors)\n", dir, issues)
	}
}
