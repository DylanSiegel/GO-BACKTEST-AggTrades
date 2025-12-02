package main

import (
	"unique"
	"unsafe"
)

// --- Shared Configuration ---

const (
	// Ryzen 9 7900X: 12 Cores / 24 Threads.
	CPUThreads = 24
	BaseDir    = "data"

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

// ParseAggRow - ZEN 4 OPTIMIZED
// Uses unsafe pointer arithmetic to bypass Go bounds checks.
// The caller GUARANTEES row has at least 48 bytes and matches the AGG3 layout.
func ParseAggRow(row []byte) AggRow {
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
