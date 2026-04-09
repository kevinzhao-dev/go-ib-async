package market

import "time"

// MktDepthData represents a market depth update.
type MktDepthData struct {
	Time        time.Time
	Position    int
	MarketMaker string
	Operation   int
	Side        int
	Price       float64
	Size        float64
}

// DOMLevel represents a level in the order book.
type DOMLevel struct {
	Price       float64
	Size        float64
	MarketMaker string
}

// HistogramData represents a histogram bucket.
type HistogramData struct {
	Price float64
	Count int
}

// DepthMktDataDescription describes market depth availability.
type DepthMktDataDescription struct {
	Exchange        string
	SecType         string
	ListingExch     string
	ServiceDataType string
	AggGroup        int
}
