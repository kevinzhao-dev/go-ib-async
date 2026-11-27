package market

import (
	"math"
	"slices"
	"sync"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/event"
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

	// NewBar is true during an UpdateEvent when the update appended a new
	// bar, false when it revised the last bar.
	NewBar bool

	mu sync.RWMutex

	// UpdateEvent fires on the client's reader goroutine after each update.
	UpdateEvent event.Event[*BarDataList]
}

// Lock acquires the write lock (used by the client while updating Bars).
func (l *BarDataList) Lock() { l.mu.Lock() }

// Unlock releases the write lock.
func (l *BarDataList) Unlock() { l.mu.Unlock() }

// RLock acquires the read lock: hold it to read fields (ReqID, NewBar,
// Bars) from a goroutine other than the client's reader goroutine.
func (l *BarDataList) RLock() { l.mu.RLock() }

// RUnlock releases the read lock.
func (l *BarDataList) RUnlock() { l.mu.RUnlock() }

// Snapshot returns a copy of the bars that is safe to use from any goroutine.
func (l *BarDataList) Snapshot() []BarData {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return slices.Clone(l.Bars)
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

	mu sync.RWMutex

	// UpdateEvent fires on the client's reader goroutine for every new bar.
	UpdateEvent event.Event[*RealTimeBarList]
}

// Lock acquires the write lock (used by the client while appending bars).
func (l *RealTimeBarList) Lock() { l.mu.Lock() }

// Unlock releases the write lock.
func (l *RealTimeBarList) Unlock() { l.mu.Unlock() }

// RLock acquires the read lock: hold it to read fields (ReqID, Bars) from a
// goroutine other than the client's reader goroutine.
func (l *RealTimeBarList) RLock() { l.mu.RLock() }

// RUnlock releases the read lock.
func (l *RealTimeBarList) RUnlock() { l.mu.RUnlock() }

// Snapshot returns a copy of the bars that is safe to use from any goroutine.
func (l *RealTimeBarList) Snapshot() []RealTimeBar {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return slices.Clone(l.Bars)
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
