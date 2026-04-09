// Package order defines order types, conditions, and trade tracking for IB.
package order

import (
	"math"

	"github.com/kvzhao/go-ib-async/contract"
)

const (
	unsetDouble  float64 = math.MaxFloat64
	unsetInteger int64   = math.MaxInt32
)

// Order represents an order for trading contracts.
type Order struct {
	OrderID                       int64
	ClientID                      int64
	PermID                        int64
	Action                        string // BUY or SELL
	TotalQuantity                 float64
	OrderType                     string // MKT, LMT, STP, STP LMT, etc.
	LmtPrice                      float64
	AuxPrice                      float64
	TIF                           string // DAY, GTC, IOC, OPG, FOK
	ActiveStartTime               string
	ActiveStopTime                string
	OcaGroup                      string
	OcaType                       int
	OrderRef                      string
	Transmit                      bool
	ParentID                      int64
	BlockOrder                    bool
	SweepToFill                   bool
	DisplaySize                   int
	TriggerMethod                 int
	OutsideRTH                    bool
	Hidden                        bool
	GoodAfterTime                 string
	GoodTillDate                  string
	Rule80A                       string
	AllOrNone                     bool
	MinQty                        int64
	PercentOffset                 float64
	OverridePercentageConstraints bool
	TrailStopPrice                float64
	TrailingPercent               float64
	FAGroup                       string
	FAMethod                      string
	FAPercentage                  string
	DesignatedLocation            string
	OpenClose                     string
	Origin                        int
	ShortSaleSlot                 int
	ExemptCode                    int
	DiscretionaryAmt              float64
	ETradeOnly                    bool
	FirmQuoteOnly                 bool
	NBBOPriceCap                  float64
	OptOutSmartRouting            bool
	AuctionStrategy               int
	StartingPrice                 float64
	StockRefPrice                 float64
	Delta                         float64
	StockRangeLower               float64
	StockRangeUpper               float64
	RandomizePrice                bool
	RandomizeSize                 bool
	Volatility                    float64
	VolatilityType                int64
	DeltaNeutralOrderType         string
	DeltaNeutralAuxPrice          float64
	DeltaNeutralConID             int64
	DeltaNeutralSettlingFirm      string
	DeltaNeutralClearingAccount   string
	DeltaNeutralClearingIntent    string
	DeltaNeutralOpenClose         string
	DeltaNeutralShortSale         bool
	DeltaNeutralShortSaleSlot     int
	DeltaNeutralDesignatedLocation string
	ContinuousUpdate              bool
	ReferencePriceType            int64
	BasisPoints                   float64
	BasisPointsType               int64
	ScaleInitLevelSize            int64
	ScaleSubsLevelSize            int64
	ScalePriceIncrement           float64
	ScalePriceAdjustValue         float64
	ScalePriceAdjustInterval      int64
	ScaleProfitOffset             float64
	ScaleAutoReset                bool
	ScaleInitPosition             int64
	ScaleInitFillQty              int64
	ScaleRandomPercent            bool
	ScaleTable                    string
	HedgeType                     string
	HedgeParam                    string
	Account                       string
	SettlingFirm                  string
	ClearingAccount               string
	ClearingIntent                string
	AlgoStrategy                  string
	AlgoParams                    []contract.TagValue
	SmartComboRoutingParams       []contract.TagValue
	AlgoID                        string
	WhatIf                        bool
	NotHeld                       bool
	Solicited                     bool
	ModelCode                     string
	OrderComboLegs                []OrderComboLeg
	OrderMiscOptions              []contract.TagValue
	ReferenceContractID           int64
	PeggedChangeAmount            float64
	IsPeggedChangeAmountDecrease  bool
	ReferenceChangeAmount         float64
	ReferenceExchangeID           string
	AdjustedOrderType             string
	TriggerPrice                  float64
	AdjustedStopPrice             float64
	AdjustedStopLimitPrice        float64
	AdjustedTrailingAmount        float64
	AdjustableTrailingUnit        int
	LmtPriceOffset                float64
	Conditions                    []OrderCondition
	ConditionsCancelOrder         bool
	ConditionsIgnoreRTH           bool
	ExtOperator                   string
	SoftDollarTier                SoftDollarTier
	CashQty                       float64
	Mifid2DecisionMaker           string
	Mifid2DecisionAlgo            string
	Mifid2ExecutionTrader         string
	Mifid2ExecutionAlgo           string
	DontUseAutoPriceForHedge      bool
	IsOmsContainer                bool
	DiscretionaryUpToLimitPrice   bool
	AutoCancelDate                string
	FilledQuantity                float64
	RefFuturesConID               int64
	AutoCancelParent              bool
	Shareholder                   string
	ImbalanceOnly                 bool
	RouteMarketableToBbo          bool
	ParentPermID                  int64
	UsePriceMgmtAlgo              bool
	Duration                      int64
	PostToAts                     int64
	AdvancedErrorOverride         string
	ManualOrderTime               string
	MinTradeQty                   int64
	MinCompeteSize                int64
	CompeteAgainstBestOffset      float64
	MidOffsetAtWhole              float64
	MidOffsetAtHalf               float64
}

// NewOrder creates an Order with IB default values.
func NewOrder() *Order {
	return &Order{
		Transmit:                 true,
		OpenClose:                "O",
		ExemptCode:               -1,
		LmtPrice:                 unsetDouble,
		AuxPrice:                 unsetDouble,
		MinQty:                   unsetInteger,
		PercentOffset:            unsetDouble,
		TrailStopPrice:           unsetDouble,
		TrailingPercent:          unsetDouble,
		NBBOPriceCap:             unsetDouble,
		StartingPrice:            unsetDouble,
		StockRefPrice:            unsetDouble,
		Delta:                    unsetDouble,
		StockRangeLower:          unsetDouble,
		StockRangeUpper:          unsetDouble,
		Volatility:               unsetDouble,
		VolatilityType:           unsetInteger,
		DeltaNeutralAuxPrice:     unsetDouble,
		ReferencePriceType:       unsetInteger,
		BasisPoints:              unsetDouble,
		BasisPointsType:          unsetInteger,
		ScaleInitLevelSize:       unsetInteger,
		ScaleSubsLevelSize:       unsetInteger,
		ScalePriceIncrement:      unsetDouble,
		ScalePriceAdjustValue:    unsetDouble,
		ScalePriceAdjustInterval: unsetInteger,
		ScaleProfitOffset:        unsetDouble,
		ScaleInitPosition:        unsetInteger,
		ScaleInitFillQty:         unsetInteger,
		TriggerPrice:             unsetDouble,
		AdjustedStopPrice:        unsetDouble,
		AdjustedStopLimitPrice:   unsetDouble,
		AdjustedTrailingAmount:   unsetDouble,
		LmtPriceOffset:           unsetDouble,
		CashQty:                  unsetDouble,
		FilledQuantity:           unsetDouble,
		Duration:                 unsetInteger,
		PostToAts:                unsetInteger,
		MinTradeQty:             unsetInteger,
		MinCompeteSize:          unsetInteger,
		CompeteAgainstBestOffset: unsetDouble,
		MidOffsetAtWhole:        unsetDouble,
		MidOffsetAtHalf:         unsetDouble,
	}
}

// LimitOrder creates a limit order.
func LimitOrder(action string, totalQuantity float64, lmtPrice float64) *Order {
	o := NewOrder()
	o.OrderType = "LMT"
	o.Action = action
	o.TotalQuantity = totalQuantity
	o.LmtPrice = lmtPrice
	return o
}

// MarketOrder creates a market order.
func MarketOrder(action string, totalQuantity float64) *Order {
	o := NewOrder()
	o.OrderType = "MKT"
	o.Action = action
	o.TotalQuantity = totalQuantity
	return o
}

// StopOrder creates a stop order.
func StopOrder(action string, totalQuantity float64, stopPrice float64) *Order {
	o := NewOrder()
	o.OrderType = "STP"
	o.Action = action
	o.TotalQuantity = totalQuantity
	o.AuxPrice = stopPrice
	return o
}

// StopLimitOrder creates a stop-limit order.
func StopLimitOrder(action string, totalQuantity float64, lmtPrice, stopPrice float64) *Order {
	o := NewOrder()
	o.OrderType = "STP LMT"
	o.Action = action
	o.TotalQuantity = totalQuantity
	o.LmtPrice = lmtPrice
	o.AuxPrice = stopPrice
	return o
}

// OrderComboLeg specifies a price for a combo leg.
type OrderComboLeg struct {
	Price float64
}

// SoftDollarTier represents a commission rebate tier.
type SoftDollarTier struct {
	Name        string
	Val         string
	DisplayName string
}

// IsSet returns true if any field is non-empty.
func (s SoftDollarTier) IsSet() bool {
	return s.Name != "" || s.Val != "" || s.DisplayName != ""
}
