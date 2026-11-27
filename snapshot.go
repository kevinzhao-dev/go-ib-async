package ibgo

import (
	"context"
	"errors"
	"sync"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/market"
)

// snapshotState tracks snapshot completions. tickSnapshotEnd is not signalled
// to the waiting caller when the message is decoded, but once the whole
// network batch has been processed (see flushSnapshotEnds): ticks of the same
// batch are stamped onto their tickers in OnDataProcessed, after the
// messages, and the caller must not see the ticker before the reader is done
// writing it.
type snapshotState struct {
	mu    sync.Mutex
	ended []int64
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

// flushSnapshotEnds completes the snapshot requests that ended in the batch
// just processed. TWS cancels snapshot requests itself, so their request ids
// are released here and the reader never writes their tickers for them again.
// Called on the reader goroutine at the end of OnDataProcessed.
func (ib *IB) flushSnapshotEnds() {
	for _, reqID := range ib.snapshots.take() {
		ib.releaseReq(reqID)
		ib.state.EndReq(reqID, nil, nil)
	}
}

// ReqTickerSnapshot requests a one-off market data snapshot of con and waits
// until TWS reports it complete (tickSnapshotEnd; TWS sends it at the latest
// about 11 seconds after the request), or until ctx ends.
//
// Unlike the tickers of ReqMktData and ReqTickers, the returned ticker is
// private to this call (not the contract's shared Ticker) and is detached
// from the client once the snapshot is complete: the reader goroutine never
// updates it again, so its fields can be read without synchronisation.
// Fields TWS did not send stay NaN. The market data type set with
// ReqMarketDataType applies.
func (ib *IB) ReqTickerSnapshot(ctx context.Context, con *contract.Contract) (*market.Ticker, error) {
	if con == nil {
		return nil, errors.New("ibgo: ReqTickerSnapshot needs a contract")
	}
	if !ib.client.IsConnected() {
		return nil, ErrNotConnected
	}
	reqID := ib.client.GetReqID()
	ticker := market.NewTicker(con)

	// Not registered in the contract index (state.Tickers): the shared
	// ticker of con stays untouched, and CancelMktData, which only matches
	// streaming subscriptions, cannot cancel this request.
	ib.state.Mu.Lock()
	ib.state.ReqID2Ticker[reqID] = ticker
	ib.state.ReqID2Contract[reqID] = con
	ib.state.Mu.Unlock()
	ch := ib.state.StartReq(reqID, con)

	if err := ib.client.ReqMktData(reqID, con, "", true, false, nil); err != nil {
		ib.state.CancelReq(reqID)
		ib.releaseReq(reqID)
		return nil, err
	}
	if _, err := ib.awaitReq(ctx, reqID, ch); err != nil {
		ib.releaseReq(reqID)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			_ = ib.client.CancelMktData(reqID)
		}
		return nil, err
	}
	return ticker, nil
}
