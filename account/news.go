package account

import "time"

// NewsProvider identifies a news source.
type NewsProvider struct {
	Code string
	Name string
}

// NewsArticle holds news article content.
type NewsArticle struct {
	ArticleType int
	ArticleText string
}

// HistoricalNews holds a historical news headline.
type HistoricalNews struct {
	Time         time.Time
	ProviderCode string
	ArticleID    string
	Headline     string
}

// NewsTick holds a real-time news tick.
type NewsTick struct {
	TimeStamp    int
	ProviderCode string
	ArticleID    string
	Headline     string
	ExtraData    string
}

// NewsBulletin holds an IB system bulletin.
type NewsBulletin struct {
	MsgID        int
	MsgType      int
	Message      string
	OrigExchange string
}

// FamilyCode holds a family account code.
type FamilyCode struct {
	AccountID     string
	FamilyCodeStr string
}

// SmartComponent holds SmartRouting component info.
type SmartComponent struct {
	BitNumber      int
	Exchange       string
	ExchangeLetter string
}

// PriceIncrement represents a market rule price increment.
type PriceIncrement struct {
	LowEdge   float64
	Increment float64
}

// HistoricalSession represents trading session bounds.
type HistoricalSession struct {
	StartDateTime string
	EndDateTime   string
	RefDate       string
}

// HistoricalSchedule holds a full trading schedule.
type HistoricalSchedule struct {
	StartDateTime string
	EndDateTime   string
	TimeZone      string
	Sessions      []HistoricalSession
}

// WshEventData holds WSH event data request parameters.
type WshEventData struct {
	ConID           int64
	Filter          string
	FillWatchlist   bool
	FillPortfolio   bool
	FillCompetitors bool
	StartDate       string
	EndDate         string
	TotalLimit      int64
}

// SoftDollarTier represents a commission rebate tier.
type SoftDollarTier struct {
	Name        string
	Val         string
	DisplayName string
}
