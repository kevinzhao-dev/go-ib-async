package ibgo

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/market"
)

// snapshotState tracks snapshot completions. tickSnapshotEnd is not signalled
// to the waiting caller when the message is decoded, but once the whole
// network batch has been processed (see flush): ticks of the same batch are
// stamped onto their tickers in OnDataProcessed, after the messages, and the
// caller must not see the ticker before the reader is done writing it.
type snapshotState struct {
	mu    sync.Mutex
	ended []int64
	// owned are the request ids of ReqTickerSnapshot calls. Their tickers are
	// detached from the request maps when the snapshot ends, so the reader
	// never writes them again.
	owned map[int64]bool
}

func (s *snapshotState) end(reqID int64) {
	s.mu.Lock()
	s.ended = append(s.ended, reqID)
	s.mu.Unlock()
}

func (s *snapshotState) take() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ended := s.ended
	s.ended = nil
	return ended
}

func (s *snapshotState) own(reqID int64, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owned == nil {
		s.owned = map[int64]bool{}
	}
	if on {
		s.owned[reqID] = true
	} else {
		delete(s.owned, reqID)
	}
}

func (s *snapshotState) isOwned(reqID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owned[reqID]
}

// flushSnapshotEnds completes the snapshot requests that ended in the batch
// just processed. Called on the reader goroutine at the end of
// OnDataProcessed.
func (ib *IB) flushSnapshotEnds() {
	for _, reqID := range ib.snapshots.take() {
		if ib.snapshots.isOwned(reqID) {
			ib.detachTicker(reqID)
		}
		ib.state.EndReq(reqID, nil, nil)
	}
}

// detachTicker removes the request mappings of a snapshot ticker so later
// messages for reqID are ignored.
func (ib *IB) detachTicker(reqID int64) {
	ib.state.Mu.Lock()
	delete(ib.state.ReqID2Ticker, reqID)
	delete(ib.state.ReqID2Contract, reqID)
	ib.state.Mu.Unlock()
}

// ReqMarketDataType selects the type of market data returned by later
// ReqMktData and ReqTickerSnapshot calls: 1 live (the default), 2 frozen,
// 3 delayed, 4 delayed-frozen. With 3 or 4 TWS falls back to delayed data for
// instruments without a live subscription. Tickers report the type they
// actually received in Ticker.MarketDataType. The setting lasts for the
// connection: call it again after a reconnect.
func (ib *IB) ReqMarketDataType(marketDataType int) error {
	if marketDataType < 1 || marketDataType > 4 {
		return fmt.Errorf("ibgo: market data type must be 1-4, got %d", marketDataType)
	}
	return ib.client.ReqMarketDataType(marketDataType)
}

// ReqTickerSnapshot requests a one-off market data snapshot of con and waits
// until TWS reports it complete (tickSnapshotEnd; TWS sends it at the latest
// about 11 seconds after the request), or until ctx ends.
//
// Unlike a ticker from ReqMktData, the returned ticker is detached from the
// client once the snapshot is complete: the reader goroutine never updates it
// again, so its fields can be read without synchronisation. Fields TWS did
// not send stay NaN.
func (ib *IB) ReqTickerSnapshot(ctx context.Context, con *contract.Contract) (*market.Ticker, error) {
	if con == nil {
		return nil, errors.New("ibgo: ReqTickerSnapshot needs a contract")
	}
	if !ib.client.IsConnected() {
		return nil, ErrNotConnected
	}
	reqID := ib.client.GetReqID()
	ticker := market.NewTicker(con)

	ib.snapshots.own(reqID, true)
	defer ib.snapshots.own(reqID, false)

	ib.state.Mu.Lock()
	ib.state.ReqID2Ticker[reqID] = ticker
	ib.state.Mu.Unlock()
	ch := ib.state.StartReq(reqID, con)

	if err := ib.client.ReqMktData(reqID, con, "", true, false, nil); err != nil {
		ib.state.CancelReq(reqID)
		ib.detachTicker(reqID)
		return nil, err
	}
	if _, err := ib.awaitReq(ctx, reqID, ch); err != nil {
		ib.detachTicker(reqID)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			_ = ib.client.CancelMktData(reqID)
		}
		return nil, err
	}
	return ticker, nil
}
