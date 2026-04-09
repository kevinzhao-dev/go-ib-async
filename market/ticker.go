// Package market defines market data types for IB.
package market

import (
	"math"
	"time"

	"github.com/kvzhao/go-ib-async/contract"
	"github.com/kvzhao/go-ib-async/event"
)

var nan = math.NaN()

// Ticker holds current market data for a contract.
type Ticker struct {
	Contract      *contract.Contract
	Time          time.Time
	Timestamp     float64
	MarketDataType int

	// Bid/Ask
	MinTick     float64
	Bid         float64
	BidSize     float64
	BidExchange string
	Ask         float64
	AskSize     float64
	AskExchange string

	// Last trade
	Last          float64
	LastSize      float64
	LastExchange  string
	LastTimestamp  time.Time
	LastYield     float64

	// Previous values
	PrevBid      float64
	PrevBidSize  float64
	PrevAsk      float64
	PrevAskSize  float64
	PrevLast     float64
	PrevLastSize float64

	// OHLCV
	Volume float64
	Open   float64
	High   float64
	Low    float64
	Close  float64
	VWAP   float64

	// Multi-week ranges
	Low13Week  float64
	High13Week float64
	Low26Week  float64
	High26Week float64
	Low52Week  float64
	High52Week float64

	// Yield
	BidYield float64
	AskYield float64

	// Mark / Halt
	MarkPrice float64
	Halted    float64

	// Real-time stats
	RTHistVolatility float64
	RTVolume         float64
	RTTradeVolume    float64
	RTTime           time.Time

	// Volume & trade stats
	AvVolume      float64
	TradeCount    float64
	TradeRate     float64
	VolumeRate    float64
	VolumeRate3Min float64
	VolumeRate5Min float64
	VolumeRate10Min float64

	// Short info
	Shortable      float64
	ShortableShares float64

	// Futures
	IndexFuturePremium  float64
	FuturesOpenInterest float64

	// Options
	PutOpenInterest   float64
	CallOpenInterest  float64
	PutVolume         float64
	CallVolume        float64
	AvOptionVolume    float64
	HistVolatility    float64
	ImpliedVolatility float64
	OpenInterest      float64

	// Last RTH trade
	LastRthTrade float64
	LastRegTime  string

	// Option exchanges
	OptionBidExch string
	OptionAskExch string

	// Bond
	BondFactorMultiplier   float64
	CreditmanMarkPrice     float64
	CreditmanSlowMarkPrice float64

	// Delayed
	DelayedLastTimestamp time.Time
	DelayedHalted       float64

	// Reuters
	ReutersMutualFunds string

	// ETF NAV
	EtfNavClose     float64
	EtfNavPriorClose float64
	EtfNavBid       float64
	EtfNavAsk       float64
	EtfNavLast      float64
	EtfFrozenNavLast float64
	EtfNavHigh      float64
	EtfNavLow       float64

	// Social
	SocialMarketAnalytics string

	// IPO
	EstimatedIpoMidpoint float64
	FinalIpoLast         float64

	// Auction
	AuctionVolume    float64
	AuctionPrice     float64
	AuctionImbalance float64
	RegulatoryImbalance float64

	// Greeks
	BidGreeks   *OptionComputation
	AskGreeks   *OptionComputation
	LastGreeks  *OptionComputation
	ModelGreeks *OptionComputation
	CustGreeks  *OptionComputation

	// EFP
	BidEfp   *EfpData
	AskEfp   *EfpData
	LastEfp  *EfpData
	OpenEfp  *EfpData
	HighEfp  *EfpData
	LowEfp   *EfpData
	CloseEfp *EfpData

	// Dividends / Fundamentals
	Dividends        *Dividends
	FundamentalRatios map[string]string

	// Tick collections
	Ticks       []TickData
	TickByTicks []interface{} // TickByTickAllLast | TickByTickBidAsk | TickByTickMidPoint
	DomBids     []DOMLevel
	DomAsks     []DOMLevel
	DomTicks    []MktDepthData

	// Metadata
	BboExchange         string
	SnapshotPermissions int

	// Event
	UpdateEvent event.Event[*Ticker]
}

// NewTicker creates a Ticker with NaN defaults for all numeric fields.
func NewTicker(c *contract.Contract) *Ticker {
	return &Ticker{
		Contract:      c,
		MarketDataType: 1,
		MinTick:       nan, Bid: nan, BidSize: nan, Ask: nan, AskSize: nan,
		Last: nan, LastSize: nan, LastYield: nan,
		PrevBid: nan, PrevBidSize: nan, PrevAsk: nan, PrevAskSize: nan,
		PrevLast: nan, PrevLastSize: nan,
		Volume: nan, Open: nan, High: nan, Low: nan, Close: nan, VWAP: nan,
		Low13Week: nan, High13Week: nan, Low26Week: nan, High26Week: nan,
		Low52Week: nan, High52Week: nan,
		BidYield: nan, AskYield: nan, MarkPrice: nan, Halted: nan,
		RTHistVolatility: nan, RTVolume: nan, RTTradeVolume: nan,
		AvVolume: nan, TradeCount: nan, TradeRate: nan,
		VolumeRate: nan, VolumeRate3Min: nan, VolumeRate5Min: nan, VolumeRate10Min: nan,
		Shortable: nan, ShortableShares: nan,
		IndexFuturePremium: nan, FuturesOpenInterest: nan,
		PutOpenInterest: nan, CallOpenInterest: nan, PutVolume: nan, CallVolume: nan,
		AvOptionVolume: nan, HistVolatility: nan, ImpliedVolatility: nan, OpenInterest: nan,
		LastRthTrade: nan,
		BondFactorMultiplier: nan, CreditmanMarkPrice: nan, CreditmanSlowMarkPrice: nan,
		DelayedHalted: nan,
		EtfNavClose: nan, EtfNavPriorClose: nan, EtfNavBid: nan, EtfNavAsk: nan,
		EtfNavLast: nan, EtfFrozenNavLast: nan, EtfNavHigh: nan, EtfNavLow: nan,
		EstimatedIpoMidpoint: nan, FinalIpoLast: nan,
		AuctionVolume: nan, AuctionPrice: nan, AuctionImbalance: nan, RegulatoryImbalance: nan,
	}
}

// IsUnset returns true if the value is NaN.
func IsUnset(value float64) bool {
	return math.IsNaN(value)
}

// HasBidAsk returns true if bid and ask are valid.
func (t *Ticker) HasBidAsk() bool {
	return t.Bid != -1 && !IsUnset(t.Bid) && t.BidSize > 0 &&
		t.Ask != -1 && !IsUnset(t.Ask) && t.AskSize > 0
}

// Midpoint returns the average of bid and ask, or NaN if not available.
func (t *Ticker) Midpoint() float64 {
	if t.HasBidAsk() {
		return (t.Bid + t.Ask) * 0.5
	}
	return nan
}

// MarketPrice returns the best available price.
func (t *Ticker) MarketPrice() float64 {
	if t.HasBidAsk() {
		if t.Bid <= t.Last && t.Last <= t.Ask {
			return t.Last
		}
		return t.Midpoint()
	}
	return t.Last
}
