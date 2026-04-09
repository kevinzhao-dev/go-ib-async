package market

import "time"

// TickData represents a level-1 tick.
type TickData struct {
	Time     time.Time
	TickType int
	Price    float64
	Size     float64
}

// HistoricalTick represents a historical trade tick.
type HistoricalTick struct {
	Time  time.Time
	Price float64
	Size  float64
}

// HistoricalTickBidAsk represents a historical bid/ask tick.
type HistoricalTickBidAsk struct {
	Time             time.Time
	TickAttribBidAsk TickAttribBidAsk
	PriceBid         float64
	PriceAsk         float64
	SizeBid          float64
	SizeAsk          float64
}

// HistoricalTickLast represents a historical last trade tick.
type HistoricalTickLast struct {
	Time              time.Time
	TickAttribLast    TickAttribLast
	Price             float64
	Size              float64
	Exchange          string
	SpecialConditions string
}

// TickByTickAllLast represents a tick-by-tick last trade.
type TickByTickAllLast struct {
	TickType          int
	Time              time.Time
	Price             float64
	Size              float64
	TickAttribLast    TickAttribLast
	Exchange          string
	SpecialConditions string
}

// TickByTickBidAsk represents a tick-by-tick bid/ask.
type TickByTickBidAsk struct {
	Time             time.Time
	BidPrice         float64
	AskPrice         float64
	BidSize          float64
	AskSize          float64
	TickAttribBidAsk TickAttribBidAsk
}

// TickByTickMidPoint represents a tick-by-tick midpoint.
type TickByTickMidPoint struct {
	Time     time.Time
	MidPoint float64
}

// TickAttrib holds flags for price/size ticks.
type TickAttrib struct {
	CanAutoExecute bool
	PastLimit      bool
	PreOpen        bool
}

// TickAttribBidAsk holds flags for bid/ask ticks.
type TickAttribBidAsk struct {
	BidPastLow  bool
	AskPastHigh bool
}

// TickAttribLast holds flags for last trade ticks.
type TickAttribLast struct {
	PastLimit  bool
	Unreported bool
}
