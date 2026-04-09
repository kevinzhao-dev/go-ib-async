// Package account defines account, portfolio, and execution types for IB.
package account

import "github.com/kvzhao/go-ib-async/contract"

// AccountValue represents an account summary value.
type AccountValue struct {
	Account   string
	Tag       string
	Value     string
	Currency  string
	ModelCode string
}

// Position represents a portfolio position.
type Position struct {
	Account  string
	Contract *contract.Contract
	Position float64
	AvgCost  float64
}

// PortfolioItem represents a position with market values.
type PortfolioItem struct {
	Contract      *contract.Contract
	Position      float64
	MarketPrice   float64
	MarketValue   float64
	AverageCost   float64
	UnrealizedPNL float64
	RealizedPNL   float64
	Account       string
}

// PnL holds daily P&L for an account.
type PnL struct {
	Account       string
	ModelCode     string
	DailyPnL      float64
	UnrealizedPnL float64
	RealizedPnL   float64
}

// PnLSingle holds P&L for a single position.
type PnLSingle struct {
	Account       string
	ModelCode     string
	ConID         int64
	DailyPnL      float64
	UnrealizedPnL float64
	RealizedPnL   float64
	Position      int
	Value         float64
}

// ScannerSubscription defines scanner request parameters.
type ScannerSubscription struct {
	NumberOfRows             int
	Instrument               string
	LocationCode             string
	ScanCode                 string
	AbovePrice               float64
	BelowPrice               float64
	AboveVolume              int
	MarketCapAbove           float64
	MarketCapBelow           float64
	MoodyRatingAbove         string
	MoodyRatingBelow         string
	SpRatingAbove            string
	SpRatingBelow            string
	MaturityDateAbove        string
	MaturityDateBelow        string
	CouponRateAbove          float64
	CouponRateBelow          float64
	ExcludeConvertible       bool
	AverageOptionVolumeAbove int
	ScannerSettingPairs      string
	StockTypeFilter          string
}

// NewScannerSubscription creates a ScannerSubscription with default values.
func NewScannerSubscription() *ScannerSubscription {
	return &ScannerSubscription{
		NumberOfRows: -1,
	}
}

// ConnectionStats holds connection performance statistics.
type ConnectionStats struct {
	StartTime    float64
	Duration     float64
	NumBytesRecv int
	NumBytesSent int
	NumMsgRecv   int
	NumMsgSent   int
}
