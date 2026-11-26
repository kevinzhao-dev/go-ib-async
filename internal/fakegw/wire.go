package fakegw

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// cursor reads client→server fields sequentially.
type cursor struct {
	f   []string
	pos int
	err error
}

func (c *cursor) str() string {
	if c.pos >= len(c.f) {
		if c.err == nil {
			c.err = fmt.Errorf("placeOrder truncated at field %d", c.pos)
		}
		return ""
	}
	v := c.f[c.pos]
	c.pos++
	return v
}

func (c *cursor) int() int64 {
	s := c.str()
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil && c.err == nil {
		c.err = fmt.Errorf("field %d: %q is not an int", c.pos-1, s)
	}
	return v
}

func (c *cursor) float() float64 {
	s := c.str()
	if s == "" {
		return math.MaxFloat64 // unset
	}
	if s == "Infinite" {
		return math.Inf(1)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil && c.err == nil {
		c.err = fmt.Errorf("field %d: %q is not a float", c.pos-1, s)
	}
	return v
}

func (c *cursor) boolean() bool { return c.str() == "1" }

func (c *cursor) skip(n int) {
	for i := 0; i < n; i++ {
		c.str()
	}
}

// parsePlaceOrder walks a placeOrder message (server version 178 layout,
// as produced by ib_async's client.placeOrder) up to the whatIf flag.
func parsePlaceOrder(f []string) (*placed, error) {
	c := &cursor{f: f, pos: 1} // skip msgId
	p := &placed{}
	p.OrderID = c.int()
	p.Contract.ConID = c.int()
	p.Contract.Symbol = c.str()
	p.Contract.SecType = c.str()
	c.skip(1) // lastTradeDateOrContractMonth
	c.skip(1) // strike
	c.skip(1) // right
	p.Contract.Multiplier = c.str()
	p.Contract.Exchange = c.str()
	c.skip(1) // primaryExchange
	p.Contract.Currency = c.str()
	p.Contract.LocalSymbol = c.str()
	p.Contract.TradingClass = c.str()
	c.skip(2) // secIdType, secId

	p.Action = c.str()
	p.Qty = c.float()
	p.OrderType = c.str()
	p.LmtPrice = c.float()
	p.AuxPrice = c.float()
	p.TIF = c.str()
	p.OcaGroup = c.str()
	p.Account = c.str()
	c.skip(2) // openClose, origin
	p.OrderRef = c.str()
	p.Transmit = c.boolean()
	p.ParentID = c.int()
	c.skip(6) // blockOrder, sweepToFill, displaySize, triggerMethod, outsideRth, hidden

	if p.Contract.SecType == "BAG" {
		n := int(c.int())
		c.skip(8 * n)
		n = int(c.int())
		c.skip(n)
		n = int(c.int())
		c.skip(2 * n)
	}

	c.skip(7) // "", discretionaryAmt, goodAfterTime, goodTillDate, faGroup, faMethod, faPercentage
	// version >= 177: no faProfile
	c.skip(22) // modelCode .. volatilityType
	dnType := c.str()
	c.skip(1) // deltaNeutralAuxPrice
	if dnType != "" {
		c.skip(8)
	}
	c.skip(6) // continuousUpdate .. scaleSubsLevelSize
	if inc := c.float(); inc > 0 && inc < math.MaxFloat64 {
		c.skip(7)
	}
	c.skip(3) // scaleTable, activeStartTime, activeStopTime
	if hedge := c.str(); hedge != "" {
		c.skip(1)
	}
	c.skip(4)        // optOutSmartRouting, clearingAccount, clearingIntent, notHeld
	if c.boolean() { // delta neutral contract
		c.skip(3)
	}
	if algo := c.str(); algo != "" {
		n := int(c.int())
		c.skip(2 * n)
	}
	c.skip(1) // algoId
	p.whatIf = c.boolean()
	if c.err != nil {
		return nil, c.err
	}
	if p.Action == "" || p.OrderType == "" {
		return nil, fmt.Errorf("placeOrder missing action/orderType")
	}
	return p, nil
}

type whatIfNumbers struct {
	init, maint, ewl string
	commission       float64
}

func price(v float64) interface{} {
	if v == math.MaxFloat64 {
		return 0.0 // TWS reports unset prices of live orders as 0
	}
	return v
}

// encodeOpenOrder builds an openOrder message (msgId 5) in the layout the
// ib_async decoder expects for server version 178.
func encodeOpenOrder(o *Order, whatIf bool, wi *whatIfNumbers) []byte {
	k := o.Contract
	unset := math.MaxFloat64
	v := []interface{}{
		5, o.OrderID,
		k.ConID, k.Symbol, k.SecType, "", 0.0, "", k.Multiplier, k.Exchange, k.Currency, k.LocalSymbol, k.TradingClass,
		o.Action, o.Qty, o.OrderType, price(o.LmtPrice), price(o.AuxPrice), o.TIF, o.OcaGroup, o.Account, "O", 0, o.OrderRef,
		o.ClientID, o.PermID, false, false, 0.0, "", "", "", "", "",
		// modelCode .. deltaNeutralAuxPrice
		"", "", "", "", "", 0, "", -1, 0, unset, unset, unset, unset, unset, 0, false, false, false, "", 0, false, false, unset,
		o.ParentID, 0, unset, "", "", unset,
		// continuousUpdate .. comboLegsDescrip
		false, "", unset, unset, unset, "", "",
		0, 0, 0, // combo legs, order combo legs, smart combo params
		"", "", "", // scaleInitLevelSize, scaleSubsLevelSize, scalePriceIncrement
		"",                          // hedgeType
		false, "", "", false, false, // optOutSmartRouting, clearingAccount, clearingIntent, notHeld, dncPresent
		"", // algoStrategy
		false, whatIf, o.Status,
	}
	if wi != nil {
		v = append(v, "0", "0", "0", wi.init, wi.maint, "0", wi.init, wi.maint, wi.ewl, wi.commission, unset, unset, "USD", "")
	} else {
		v = append(v, unset, unset, unset, unset, unset, unset, unset, unset, unset, unset, unset, unset, "", "")
	}
	v = append(v,
		false, false, // randomizeSize, randomizePrice
		0,                                               // numConditions
		"", unset, unset, unset, unset, unset, unset, 0, // adjusted order fields
		"", "", "", // soft dollar tier
		unset, false, false, false, false, // cashQty .. usePriceMgmtAlgo
		"", "", false, // duration, postToAts, autoCancelParent
		"", "", unset, unset, unset, // minTradeQty .. midOffsetAtHalf
	)
	return msg(v...)
}

// encodeCompletedOrder builds a completedOrder message (msgId 101).
func encodeCompletedOrder(o *Order) []byte {
	k := o.Contract
	unset := math.MaxFloat64
	v := []interface{}{
		101,
		k.ConID, k.Symbol, k.SecType, "", 0.0, "", k.Multiplier, k.Exchange, k.Currency, k.LocalSymbol, k.TradingClass,
		o.Action, o.Qty, o.OrderType, price(o.LmtPrice), price(o.AuxPrice), o.TIF, o.OcaGroup, o.Account, "O", 0, o.OrderRef,
		o.PermID, false, false, 0.0, "", "", "", "",
		// modelCode .. deltaNeutralAuxPrice
		"", "", "", unset, "", 0, "", -1, unset, unset, unset, unset, unset, 0, false, false, "", 0, 0, unset, "", "", unset,
		// continuousUpdate .. comboLegsDescrip
		false, "", unset, unset, "",
		0, 0, 0,
		"", "", "",
		"",
		"", "", false, false, // clearingAccount, clearingIntent, notHeld, dncPresent
		"",
		false, o.Status, false, false, // solicited, status, randomizeSize, randomizePrice
		0, // numConditions
		unset, unset, unset, false, false, "", o.Filled, 0, false, "", false, false, 0,
		"20261126 10:00:00 America/New_York", o.Status,
		"", "", unset, unset, unset,
	}
	return msg(v...)
}

// encodeOrderStatus builds an orderStatus message (msgId 3, no version).
func encodeOrderStatus(o *Order) []byte {
	return msg(3, o.OrderID, o.Status, o.Filled, o.Remaining(), o.AvgPrice, o.PermID, o.ParentID, o.AvgPrice, o.ClientID, "", 0.0)
}

// encodeExecDetails builds an execDetails message (msgId 11, no version).
func encodeExecDetails(reqID int64, ex *execution) []byte {
	k := ex.Contract
	return msg(11, reqID, ex.OrderID,
		k.ConID, k.Symbol, k.SecType, "", 0.0, "", k.Multiplier, k.Exchange, k.Currency, k.LocalSymbol, k.TradingClass,
		ex.ExecID, ex.Time.In(nyc).Format("20060102 15:04:05")+" America/New_York", ex.Account, "ISLAND", ex.Side,
		ex.Shares, ex.Price, ex.PermID, ex.ClientID, 0, ex.CumQty, ex.AvgPrice, "", "", "", "", 1,
		false, // pendingPriceRevision (server >= 178)
	)
}

// encodeCommission builds a commissionReport message (msgId 59, version 1).
func encodeCommission(ex *execution) []byte {
	realized := math.MaxFloat64 // TWS sends UNSET when nothing was realized
	if ex.Realized != 0 {
		realized = ex.Realized - ex.Commission
	}
	return msg(59, 1, ex.ExecID, ex.Commission, "USD", realized, math.MaxFloat64, "")
}

var nyc = func() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.UTC
}()
