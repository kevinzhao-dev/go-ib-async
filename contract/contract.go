// Package contract defines financial instrument types used by Interactive Brokers.
package contract

import (
	"fmt"
	"sort"
)

// TagValue is a generic tag-value pair used for various IB parameters.
type TagValue struct {
	Tag   string
	Value string
}

// ComboLeg represents a leg in a combo (BAG) contract.
type ComboLeg struct {
	ConID              int64
	Ratio              int
	Action             string
	Exchange           string
	OpenClose          int
	ShortSaleSlot      int
	DesignatedLocation string
	ExemptCode         int
}

// NewComboLeg returns a ComboLeg with default ExemptCode = -1.
func NewComboLeg() ComboLeg {
	return ComboLeg{ExemptCode: -1}
}

// DeltaNeutralContract holds delta and price for delta-neutral combo orders.
type DeltaNeutralContract struct {
	ConID int64
	Delta float64
	Price float64
}

// Contract represents a financial instrument.
type Contract struct {
	SecType                      string
	ConID                        int64
	Symbol                       string
	LastTradeDateOrContractMonth string
	Strike                       float64
	Right                        string
	Multiplier                   string
	Exchange                     string
	PrimaryExchange              string
	Currency                     string
	LocalSymbol                  string
	TradingClass                 string
	IncludeExpired               bool
	SecIDType                    string
	SecID                        string
	Description                  string
	IssuerID                     string
	ComboLegsDescrip             string
	ComboLegs                    []ComboLeg
	DeltaNeutralContract         *DeltaNeutralContract
}

// IsHashable returns true if this contract can be used as a map key by ConID.
// BAG contracts always get ConID=28812380, so they use a synthetic hash.
func (c *Contract) IsHashable() bool {
	return c.ConID != 0
}

// Key returns a comparable value for use as a map key.
// BAG contracts use a synthetic key from legs; CONTFUT inverts ConID.
func (c *Contract) Key() int64 {
	if c.SecType == "BAG" {
		return c.bagHash()
	}
	if c.SecType == "CONTFUT" {
		return -c.ConID
	}
	return c.ConID
}

func (c *Contract) bagHash() int64 {
	if len(c.ComboLegs) == 0 {
		return 0
	}
	sorted := make([]ComboLeg, len(c.ComboLegs))
	copy(sorted, c.ComboLegs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ConID < sorted[j].ConID
	})
	// Simple hash combining leg conIds, ratios, actions
	var h int64
	for _, leg := range sorted {
		h = h*31 + leg.ConID
		h = h*31 + int64(leg.Ratio)
	}
	return h
}

// Equal checks contract equality by ConID (if set) or all fields.
func (c *Contract) Equal(other *Contract) bool {
	if other == nil {
		return false
	}
	if c.ConID != 0 && c.ConID == other.ConID {
		return true
	}
	return c.SecType == other.SecType &&
		c.Symbol == other.Symbol &&
		c.Exchange == other.Exchange &&
		c.Currency == other.Currency &&
		c.LastTradeDateOrContractMonth == other.LastTradeDateOrContractMonth &&
		c.Strike == other.Strike &&
		c.Right == other.Right &&
		c.Multiplier == other.Multiplier &&
		c.LocalSymbol == other.LocalSymbol &&
		c.TradingClass == other.TradingClass
}

func (c *Contract) String() string {
	if c.LocalSymbol != "" {
		return fmt.Sprintf("%s(%s, %s, %s)", c.SecType, c.LocalSymbol, c.Exchange, c.Currency)
	}
	return fmt.Sprintf("%s(%s, %s, %s)", c.SecType, c.Symbol, c.Exchange, c.Currency)
}

// --- Constructor Functions ---

// Stock creates a stock/ETF contract.
func Stock(symbol, exchange, currency string) *Contract {
	return &Contract{SecType: "STK", Symbol: symbol, Exchange: exchange, Currency: currency}
}

// Option creates an option contract.
func Option(symbol, lastTradeDate string, strike float64, right, exchange string) *Contract {
	return &Contract{
		SecType:                      "OPT",
		Symbol:                       symbol,
		LastTradeDateOrContractMonth: lastTradeDate,
		Strike:                       strike,
		Right:                        right,
		Exchange:                     exchange,
	}
}

// Future creates a futures contract.
func Future(symbol, lastTradeDate, exchange string) *Contract {
	return &Contract{
		SecType:                      "FUT",
		Symbol:                       symbol,
		LastTradeDateOrContractMonth: lastTradeDate,
		Exchange:                     exchange,
	}
}

// ContFuture creates a continuous futures contract.
func ContFuture(symbol, exchange string) *Contract {
	return &Contract{SecType: "CONTFUT", Symbol: symbol, Exchange: exchange}
}

// Forex creates a currency pair contract. pair should be 6 chars, e.g. "EURUSD".
func Forex(pair string) *Contract {
	if len(pair) == 6 {
		return &Contract{
			SecType:  "CASH",
			Symbol:   pair[:3],
			Currency: pair[3:],
			Exchange: "IDEALPRO",
		}
	}
	return &Contract{SecType: "CASH", Exchange: "IDEALPRO"}
}

// Index creates an index contract.
func Index(symbol, exchange, currency string) *Contract {
	return &Contract{SecType: "IND", Symbol: symbol, Exchange: exchange, Currency: currency}
}

// CFD creates a Contract For Difference.
func CFD(symbol, exchange, currency string) *Contract {
	return &Contract{SecType: "CFD", Symbol: symbol, Exchange: exchange, Currency: currency}
}

// Commodity creates a commodity contract.
func Commodity(symbol, exchange, currency string) *Contract {
	return &Contract{SecType: "CMDTY", Symbol: symbol, Exchange: exchange, Currency: currency}
}

// Bond creates a bond contract.
func Bond() *Contract {
	return &Contract{SecType: "BOND"}
}

// FuturesOption creates an option on a futures contract.
func FuturesOption(symbol, lastTradeDate string, strike float64, right, exchange string) *Contract {
	return &Contract{
		SecType:                      "FOP",
		Symbol:                       symbol,
		LastTradeDateOrContractMonth: lastTradeDate,
		Strike:                       strike,
		Right:                        right,
		Exchange:                     exchange,
	}
}

// MutualFund creates a mutual fund contract.
func MutualFund() *Contract {
	return &Contract{SecType: "FUND"}
}

// Warrant creates a warrant contract.
func Warrant() *Contract {
	return &Contract{SecType: "WAR"}
}

// Bag creates a combo/bag contract.
func Bag() *Contract {
	return &Contract{SecType: "BAG"}
}

// Crypto creates a cryptocurrency contract.
func Crypto(symbol, exchange, currency string) *Contract {
	return &Contract{SecType: "CRYPTO", Symbol: symbol, Exchange: exchange, Currency: currency}
}
