package main

import (
	"encoding/binary"
)

// --- Shared Configuration ---
const (
	CPUThreads = 24
	Symbol     = "BTCUSDT"
	BaseDir    = "data"

	// Binary Layout
	PxScale    = 100_000_000.0
	QtScale    = 100_000_000.0
	HeaderSize = 48
	RowSize    = 48

	// Magic Headers
	AggMagic   = "AGG3"
	IdxMagic   = "QIDX"
	IdxVersion = 1 // Index file format version
)

// --- Zero-Alloc Trade Parsing ---

type AggRow struct {
	TsMs       int64
	PriceFixed uint64
	QtyFixed   uint64
	Flags      uint16
}

// ParseAggRow interprets a 48-byte row from data.quantdev without allocation.
func ParseAggRow(row []byte) AggRow {
	return AggRow{
		TsMs:       int64(binary.LittleEndian.Uint64(row[38:])),
		PriceFixed: binary.LittleEndian.Uint64(row[8:]),
		QtyFixed:   binary.LittleEndian.Uint64(row[16:]),
		Flags:      binary.LittleEndian.Uint16(row[36:]),
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

// TradeSign returns +1 for taker buy, -1 for taker sell.
func TradeSign(row AggRow) float64 {
	if row.Flags&1 != 0 {
		// buyer is maker -> taker is seller
		return -1.0
	}
	return 1.0
}
