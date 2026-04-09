package market

import (
	"math"
	"time"

	"github.com/kvzhao/go-ib-async/contract"
	"github.com/kvzhao/go-ib-async/event"
)

// BarData represents an OHLCV bar.
type BarData struct {
	Date     time.Time
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
	Average  float64
	BarCount int
}

// RealTimeBar represents a real-time bar (5-second).
type RealTimeBar struct {
	Time    time.Time
	EndTime int
	Open    float64
	High    float64
	Low     float64
	Close   float64
	Volume  float64
	WAP     float64
	Count   int
}

// BarDataList is a container for bars with request metadata.
type BarDataList struct {
	Bars           []BarData
	ReqID          int64
	Contract       *contract.Contract
	EndDateTime    string
	DurationStr    string
	BarSizeSetting string
	WhatToShow     string
	UseRTH         bool
	FormatDate     int
	KeepUpToDate   bool
	ChartOptions   []contract.TagValue

	UpdateEvent event.Event[*BarDataList]
}

// RealTimeBarList is a container for real-time bars with metadata.
type RealTimeBarList struct {
	Bars                []RealTimeBar
	ReqID               int64
	Contract            *contract.Contract
	BarSize             int
	WhatToShow          string
	UseRTH              bool
	RealTimeBarsOptions []contract.TagValue

	UpdateEvent event.Event[*RealTimeBarList]
}

// Bar is an aggregated bar from tick streams.
type Bar struct {
	Time   time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume int
	Count  int
}

// NewBar creates a Bar with NaN defaults.
func NewBar(t time.Time) *Bar {
	return &Bar{
		Time:  t,
		Open:  math.NaN(),
		High:  math.NaN(),
		Low:   math.NaN(),
		Close: math.NaN(),
	}
}
