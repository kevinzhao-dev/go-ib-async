package protocol

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
)

// ClientState represents the connection state.
type ClientState int

const (
	Disconnected ClientState = iota
	Connecting
	Connected
)

// Client manages the IB TWS connection, handshake, message sending and reading.
type Client struct {
	Conn          *Connection
	Decoder       *Decoder
	Host          string
	Port          int
	ClientID      int
	ServerVersion int
	ConnState     ClientState
	OptCapab      string

	reqIDSeq  atomic.Int64
	accounts  []string
	hasReqID  bool
	apiReady  bool
	startTime time.Time

	// handler receives decoded messages
	handler Handler

	done   chan struct{}
	mu     sync.Mutex
	readWg sync.WaitGroup

	// OnReady is called when handshake completes (nextValidId + managedAccounts received)
	OnReady func()

	// OnDisconnect is called when connection is lost
	OnDisconnect func(err error)

	// OnMessage is called for every decoded message (after handshake)
	OnMessage Handler
}

// NewClient creates a new Client.
func NewClient() *Client {
	c := &Client{
		Conn: &Connection{},
		done: make(chan struct{}),
	}
	return c
}

// Connect establishes a connection to TWS/Gateway and performs the handshake.
func (c *Client) Connect(ctx context.Context, host string, port, clientID int) error {
	c.Host = host
	c.Port = port
	c.ClientID = clientID
	c.ConnState = Connecting
	c.hasReqID = false
	c.apiReady = false
	c.accounts = nil
	c.startTime = time.Now()
	c.done = make(chan struct{})

	if err := c.Conn.Connect(host, port); err != nil {
		c.ConnState = Disconnected
		return fmt.Errorf("connect to %s:%d: %w", host, port, err)
	}

	// Send handshake: "API\0" + length-prefixed version string
	versionStr := fmt.Sprintf("v%d..%d", MinClientVersion, MaxClientVersion)
	if c.OptCapab != "" {
		versionStr += " " + c.OptCapab
	}
	versionBytes := []byte(versionStr)
	prefix := make([]byte, 4)
	binary.BigEndian.PutUint32(prefix, uint32(len(versionBytes)))

	handshake := append([]byte("API\x00"), prefix...)
	handshake = append(handshake, versionBytes...)
	if err := c.Conn.SendRaw(handshake); err != nil {
		c.Disconnect()
		return fmt.Errorf("send handshake: %w", err)
	}

	// Start read loop
	readyCh := make(chan struct{}, 1)
	c.OnReady = func() {
		select {
		case readyCh <- struct{}{}:
		default:
		}
	}

	c.readWg.Add(1)
	go c.readLoop()

	// Wait for API ready (nextValidId + managedAccounts) or context cancellation
	select {
	case <-readyCh:
		return nil
	case <-c.done:
		return fmt.Errorf("connection closed during handshake")
	case <-ctx.Done():
		c.Disconnect()
		return ctx.Err()
	}
}

// Disconnect closes the connection.
func (c *Client) Disconnect() {
	c.mu.Lock()
	c.ConnState = Disconnected
	c.apiReady = false
	c.mu.Unlock()

	c.Conn.Disconnect()

	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// IsConnected returns true if connected.
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ConnState == Connected
}

// IsReady returns true if the API handshake is complete.
func (c *Client) IsReady() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apiReady
}

// GetReqID returns a new unique request ID.
func (c *Client) GetReqID() int64 {
	return c.reqIDSeq.Add(1) - 1
}

// UpdateReqID ensures the next request ID is at least minReqID.
func (c *Client) UpdateReqID(minReqID int64) {
	for {
		current := c.reqIDSeq.Load()
		if current >= minReqID {
			return
		}
		if c.reqIDSeq.CompareAndSwap(current, minReqID) {
			return
		}
	}
}

// Accounts returns the managed accounts list.
func (c *Client) Accounts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]string, len(c.accounts))
	copy(result, c.accounts)
	return result
}

// Done returns a channel that's closed when the connection ends.
func (c *Client) Done() <-chan struct{} {
	return c.done
}

// Send serializes and sends a message with mixed field types.
func (c *Client) Send(fields ...interface{}) error {
	msg := BuildMessage(fields...)
	return c.Conn.SendMessage(msg)
}

// readLoop is the main reader goroutine.
func (c *Client) readLoop() {
	defer c.readWg.Done()
	defer func() {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
		if c.OnDisconnect != nil {
			c.OnDisconnect(nil)
		}
	}()

	for {
		fields, err := c.Conn.ReadMessage()
		if err != nil {
			if c.ConnState != Disconnected {
				log.Printf("ibgo: read error: %v", err)
			}
			return
		}

		// Handshake: first message is [version, connTime]
		if c.ServerVersion == 0 && len(fields) == 2 {
			version, err := strconv.Atoi(fields[0])
			if err != nil {
				log.Printf("ibgo: invalid server version: %s", fields[0])
				return
			}
			if version < MinClientVersion {
				log.Printf("ibgo: server version %d < minimum %d", version, MinClientVersion)
				return
			}
			c.ServerVersion = version
			c.mu.Lock()
			c.ConnState = Connected
			c.mu.Unlock()

			// Send startApi
			c.Send(MsgStartApi, 2, c.ClientID, c.OptCapab)
			continue
		}

		// Snoop for nextValidId (9) and managedAccounts (15) before API is ready
		if !c.apiReady && len(fields) >= 3 {
			msgID, _ := strconv.Atoi(fields[0])
			if msgID == InMsgNextValidID {
				validID, _ := strconv.ParseInt(fields[2], 10, 64)
				c.UpdateReqID(validID)
				c.hasReqID = true
			} else if msgID == InMsgManagedAccounts {
				accts := fields[2]
				c.mu.Lock()
				c.accounts = splitAccounts(accts)
				c.mu.Unlock()
			}

			if c.hasReqID && len(c.Accounts()) > 0 {
				c.mu.Lock()
				c.apiReady = true
				c.mu.Unlock()
				if c.OnReady != nil {
					c.OnReady()
				}
			}
		}

		// Dispatch to message handler
		if c.OnMessage != nil {
			if len(fields) > 0 {
				msgID, _ := strconv.Atoi(fields[0])
				reader := NewFieldReader(fields)
				reader.Skip(1)
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("ibgo: panic in handler for msgID %d: %v", msgID, r)
						}
					}()
					c.OnMessage(msgID, reader)
				}()
			}
		}
	}
}

func splitAccounts(s string) []string {
	var accounts []string
	for _, a := range strings.Split(s, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			accounts = append(accounts, a)
		}
	}
	return accounts
}

// --- Outbound request methods ---

func (c *Client) StartApi() error {
	return c.Send(MsgStartApi, 2, c.ClientID, c.OptCapab)
}

func (c *Client) ReqMktData(reqID int64, con *contract.Contract, genericTickList string, snapshot, regulatorySnapshot bool, mktDataOptions []contract.TagValue) error {
	fields := []interface{}{MsgReqMktData, 11, reqID, con}

	if con.SecType == "BAG" {
		legs := con.ComboLegs
		fields = append(fields, len(legs))
		for _, leg := range legs {
			fields = append(fields, leg.ConID, leg.Ratio, leg.Action, leg.Exchange)
		}
	}

	if con.DeltaNeutralContract != nil {
		dnc := con.DeltaNeutralContract
		fields = append(fields, true, dnc.ConID, dnc.Delta, dnc.Price)
	} else {
		fields = append(fields, false)
	}

	fields = append(fields, genericTickList, snapshot, regulatorySnapshot, mktDataOptions)
	return c.Send(fields...)
}

func (c *Client) CancelMktData(reqID int64) error {
	return c.Send(MsgCancelMktData, 2, reqID)
}

func (c *Client) ReqContractDetails(reqID int64, con *contract.Contract) error {
	fields := []interface{}{
		MsgReqContractDetails, 8, reqID, con,
		con.IncludeExpired, con.SecIDType, con.SecID,
	}
	if c.ServerVersion >= 176 {
		fields = append(fields, con.IssuerID)
	}
	return c.Send(fields...)
}

func (c *Client) ReqHistoricalData(reqID int64, con *contract.Contract, endDateTime, durationStr, barSizeSetting, whatToShow string, useRTH bool, formatDate int, keepUpToDate bool, chartOptions []contract.TagValue) error {
	fields := []interface{}{
		MsgReqHistoricalData, reqID, con, con.IncludeExpired,
		endDateTime, barSizeSetting, durationStr, useRTH,
		whatToShow, formatDate,
	}

	if con.SecType == "BAG" {
		legs := con.ComboLegs
		fields = append(fields, len(legs))
		for _, leg := range legs {
			fields = append(fields, leg.ConID, leg.Ratio, leg.Action, leg.Exchange)
		}
	}

	fields = append(fields, keepUpToDate, chartOptions)
	return c.Send(fields...)
}

func (c *Client) CancelHistoricalData(reqID int64) error {
	return c.Send(MsgCancelHistoricalData, 1, reqID)
}

func (c *Client) ReqOpenOrders() error {
	return c.Send(MsgReqOpenOrders, 1)
}

func (c *Client) ReqAccountUpdates(subscribe bool, acctCode string) error {
	return c.Send(MsgReqAccountUpdates, 2, subscribe, acctCode)
}

func (c *Client) ReqExecutions(reqID int64, filter ExecutionFilter) error {
	return c.Send(
		MsgReqExecutions, 3, reqID,
		filter.ClientID, filter.AcctCode, filter.Time,
		filter.Symbol, filter.SecType, filter.Exchange, filter.Side,
	)
}

func (c *Client) ReqIds(numIds int) error {
	return c.Send(MsgReqIds, 1, numIds)
}

func (c *Client) ReqPositions() error {
	return c.Send(MsgReqPositions, 1)
}

func (c *Client) CancelPositions() error {
	return c.Send(MsgCancelPositions, 1)
}

func (c *Client) ReqAccountSummary(reqID int64, groupName, tags string) error {
	return c.Send(MsgReqAccountSummary, 1, reqID, groupName, tags)
}

func (c *Client) CancelAccountSummary(reqID int64) error {
	return c.Send(MsgCancelAccountSummary, 1, reqID)
}

func (c *Client) CancelOrder(orderID int64, manualCancelOrderTime string) error {
	fields := []interface{}{MsgCancelOrder, 1, orderID}
	if c.ServerVersion >= 169 {
		fields = append(fields, manualCancelOrderTime)
	}
	return c.Send(fields...)
}

func (c *Client) ReqGlobalCancel() error {
	return c.Send(MsgReqGlobalCancel, 1)
}

func (c *Client) ReqAllOpenOrders() error {
	return c.Send(MsgReqAllOpenOrders, 1)
}

func (c *Client) ReqAutoOpenOrders(bAutoBind bool) error {
	return c.Send(MsgReqAutoOpenOrders, 1, bAutoBind)
}

func (c *Client) ReqCurrentTime() error {
	return c.Send(MsgReqCurrentTime, 1)
}

func (c *Client) ReqCompletedOrders(apiOnly bool) error {
	return c.Send(MsgReqCompletedOrders, apiOnly)
}

func (c *Client) ReqMatchingSymbols(reqID int64, pattern string) error {
	return c.Send(MsgReqMatchingSymbols, reqID, pattern)
}

func (c *Client) ReqMarketDataType(marketDataType int) error {
	return c.Send(MsgReqMarketDataType, 1, marketDataType)
}

func (c *Client) ReqMarketRule(marketRuleID int) error {
	return c.Send(MsgReqMarketRule, marketRuleID)
}

func (c *Client) ReqPnL(reqID int64, account, modelCode string) error {
	return c.Send(MsgReqPnL, reqID, account, modelCode)
}

func (c *Client) CancelPnL(reqID int64) error {
	return c.Send(MsgCancelPnL, reqID)
}

func (c *Client) ReqPnLSingle(reqID int64, account, modelCode string, conID int64) error {
	return c.Send(MsgReqPnLSingle, reqID, account, modelCode, conID)
}

func (c *Client) CancelPnLSingle(reqID int64) error {
	return c.Send(MsgCancelPnLSingle, reqID)
}

func (c *Client) ReqSecDefOptParams(reqID int64, underlyingSymbol, futFopExchange, underlyingSecType string, underlyingConID int64) error {
	return c.Send(MsgReqSecDefOptParams, reqID, underlyingSymbol, futFopExchange, underlyingSecType, underlyingConID)
}

func (c *Client) ReqNewsProviders() error {
	return c.Send(MsgReqNewsProviders)
}

func (c *Client) ReqNewsArticle(reqID int64, providerCode, articleID string, newsArticleOptions []contract.TagValue) error {
	return c.Send(MsgReqNewsArticle, reqID, providerCode, articleID, newsArticleOptions)
}

func (c *Client) ReqHistoricalNews(reqID int64, conID int64, providerCodes, startDateTime, endDateTime string, totalResults int, historicalNewsOptions []contract.TagValue) error {
	return c.Send(MsgReqHistoricalNews, reqID, conID, providerCodes, startDateTime, endDateTime, totalResults, historicalNewsOptions)
}

func (c *Client) ReqHeadTimestamp(reqID int64, con *contract.Contract, whatToShow string, useRTH bool, formatDate int) error {
	return c.Send(MsgReqHeadTimestamp, reqID, con, con.IncludeExpired, whatToShow, useRTH, formatDate)
}

func (c *Client) CancelHeadTimestamp(reqID int64) error {
	return c.Send(MsgCancelHeadTimestamp, reqID)
}

func (c *Client) ReqRealTimeBars(reqID int64, con *contract.Contract, barSize int, whatToShow string, useRTH bool, realTimeBarsOptions []contract.TagValue) error {
	return c.Send(MsgReqRealTimeBars, 3, reqID, con, barSize, whatToShow, useRTH, realTimeBarsOptions)
}

func (c *Client) CancelRealTimeBars(reqID int64) error {
	return c.Send(MsgCancelRealTimeBars, 1, reqID)
}

func (c *Client) ReqScannerSubscription(reqID int64, sub ScannerSubscription, scannerSubOptions, scannerSubFilterOptions []contract.TagValue) error {
	return c.Send(
		MsgReqScannerSubscription, reqID,
		sub.NumberOfRows, sub.Instrument, sub.LocationCode, sub.ScanCode,
		sub.AbovePrice, sub.BelowPrice, sub.AboveVolume,
		sub.MarketCapAbove, sub.MarketCapBelow,
		sub.MoodyRatingAbove, sub.MoodyRatingBelow,
		sub.SpRatingAbove, sub.SpRatingBelow,
		sub.MaturityDateAbove, sub.MaturityDateBelow,
		sub.CouponRateAbove, sub.CouponRateBelow,
		sub.ExcludeConvertible, sub.AverageOptionVolumeAbove,
		sub.ScannerSettingPairs, sub.StockTypeFilter,
		scannerSubOptions, scannerSubFilterOptions,
	)
}

func (c *Client) CancelScannerSubscription(reqID int64) error {
	return c.Send(MsgCancelScannerSubscription, 1, reqID)
}

func (c *Client) ReqScannerParameters() error {
	return c.Send(MsgReqScannerParameters, 1)
}

func (c *Client) ReqFamilyCodes() error {
	return c.Send(MsgReqFamilyCodes)
}

func (c *Client) ReqSmartComponents(reqID int64, bboExchange string) error {
	return c.Send(MsgReqSmartComponents, reqID, bboExchange)
}

func (c *Client) ReqMktDepthExchanges() error {
	return c.Send(MsgReqMktDepthExchanges)
}

func (c *Client) ReqSoftDollarTiers(reqID int64) error {
	return c.Send(MsgReqSoftDollarTiers, reqID)
}

func (c *Client) ReqHistogramData(reqID int64, con *contract.Contract, useRTH bool, timePeriod string) error {
	return c.Send(MsgReqHistogramData, reqID, con, con.IncludeExpired, useRTH, timePeriod)
}

func (c *Client) CancelHistogramData(reqID int64) error {
	return c.Send(MsgCancelHistogramData, reqID)
}

func (c *Client) ReqUserInfo(reqID int64) error {
	return c.Send(MsgReqUserInfo, reqID)
}

// ExecutionFilter for reqExecutions.
type ExecutionFilter struct {
	ClientID int64
	AcctCode string
	Time     string
	Symbol   string
	SecType  string
	Exchange string
	Side     string
}

// ScannerSubscription for reqScannerSubscription.
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

// MinClientVersion and MaxClientVersion for the protocol.
const (
	MinClientVersion = 157
	MaxClientVersion = 178
)
