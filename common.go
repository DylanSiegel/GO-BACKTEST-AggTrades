package main

import (
	"runtime"
	"unique"
	"unsafe"
)

// --- Shared Configuration ---

var (
	// Dynamic CPU detection with balanced cap for 24GB systems
	// 8 workers = ~6-8GB peak (sweet spot for performance/safety)
	CPUThreads = func() int {
		cores := runtime.NumCPU()
		maxWorkers := 8 // Balanced limit for 24GB RAM (leaves 16GB free)
		if cores > maxWorkers {
			return maxWorkers
		}
		return cores
	}()
	BaseDir = "data"
)

	// Binary Layout Constants
	PxScale    = 100_000_000.0
	QtScale    = 100_000_000.0
	HeaderSize = 48
	RowSize    = 48

	// Magic Headers
	AggMagic   = "AGG3"
	IdxMagic   = "QIDX"
	IdxVersion = 1
)

// Intern the symbol to keep it in L3 cache.
var SymbolHandle = unique.Make("ETHUSDT")

func Symbol() string { return SymbolHandle.Value() }

// AggRow corresponds to the logical fields stored in a 48-byte row.
type AggRow struct {
	TsMs       int64
	PriceFixed uint64
	QtyFixed   uint64
	Flags      uint16
}

// ParseAggRow - ARM64/NEON OPTIMIZED
// Uses unsafe pointer arithmetic.
// The caller GUARANTEES row has at least 48 bytes.
func ParseAggRow(row []byte) AggRow {
	// Identify pointer to the start of the slice
	ptr := unsafe.Pointer(&row[0])

	return AggRow{
		// Offset 38: Timestamp (uint64)
		TsMs: int64(*(*uint64)(unsafe.Add(ptr, 38))),
		// Offset 8: Price (fixed-point, 1e-8)
		PriceFixed: *(*uint64)(unsafe.Add(ptr, 8)),
		// Offset 16: Quantity (fixed-point, 1e-8)
		QtyFixed: *(*uint64)(unsafe.Add(ptr, 16)),
		// Offset 36: Flags (uint16), bit 0 encodes is_buyer_maker.
		Flags: *(*uint16)(unsafe.Add(ptr, 36)),
	}
}

func TradePrice(row AggRow) float64 {
	return float64(row.PriceFixed) / PxScale
}

func TradeQty(row AggRow) float64 {
	return float64(row.QtyFixed) / QtScale
}

func TradeDollar(row AggRow) float64 {
	return TradePrice(row) * TradeQty(row)
}

// TradeSign:
//
//	Flags&1 == 1  -> is_buyer_maker == true -> seller-initiated -> -1
//	Flags&1 == 0  -> is_buyer_maker == false -> buyer-initiated  -> +1
func TradeSign(row AggRow) float64 {
	if row.Flags&1 != 0 {
		return -1.0
	}
	return 1.0
}
