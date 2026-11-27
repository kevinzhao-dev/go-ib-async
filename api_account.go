package ibgo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

// defaultAccount resolves "" to the only managed account.
func (ib *IB) defaultAccount(acct string) (string, error) {
	if acct != "" {
		return acct, nil
	}
	accts := ib.ManagedAccounts()
	if len(accts) == 1 {
		return accts[0], nil
	}
	return "", fmt.Errorf("ibgo: account required (managed accounts: %v)", accts)
}

// --- Account updates & portfolio ---

// ReqAccountUpdates subscribes to account values and portfolio updates of
// one account ("" = the only managed account) and waits until the initial
// download is complete. Afterwards AccountValues() and Portfolio() stay up
// to date and AccountValueEvent / PortfolioEvent fire on changes. When a
// download completes, positions of the account it did not report (closed
// meanwhile) are removed from Portfolio().
//
// TWS supports one such subscription at a time (per API client; error 2101
// when another client holds one for a different account fails the call);
// it is re-sent after an automatic reconnect. A CancelAccountUpdates while
// the download is running ends the call with ErrRequestCancelled.
func (ib *IB) ReqAccountUpdates(ctx context.Context, acct string) error {
	acct, err := ib.defaultAccount(acct)
	if err != nil {
		return err
	}
	var mine *string
	_, err = ib.runSingle(ctx, ib.acctReq, false, func() error {
		ib.acctMu.Lock()
		defer ib.acctMu.Unlock()
		ib.beginAcctDownload(acct)
		if err := ib.client.ReqAccountUpdates(true, acct); err != nil {
			return err
		}
		// Recorded with the send, so a cancel or restore that follows is
		// ordered after it.
		mine = &acct
		ib.acctUpdates = mine
		return nil
	})
	if err != nil && mine != nil {
		// Failed or abandoned after subscribing: unsubscribe, unless the
		// subscription was cancelled or replaced meanwhile.
		ib.acctMu.Lock()
		if ib.acctUpdates == mine {
			ib.acctUpdates = nil
			_ = ib.client.ReqAccountUpdates(false, acct)
		}
		ib.acctMu.Unlock()
	}
	return err
}

// CancelAccountUpdates stops the ReqAccountUpdates subscription.
func (ib *IB) CancelAccountUpdates(acct string) error {
	acct, err := ib.defaultAccount(acct)
	if err != nil {
		return err
	}
	ib.acctMu.Lock()
	defer ib.acctMu.Unlock()
	ib.acctUpdates = nil
	ib.dlMu.Lock()
	ib.acctDL = nil
	ib.dlMu.Unlock()
	err = ib.client.ReqAccountUpdates(false, acct)
	// A download still running belongs to the cancelled subscription.
	if key := ib.acctReq.active(); key != 0 {
		ib.state.EndReq(key, nil, ErrRequestCancelled)
	}
	return err
}

// acctDownload tracks a running account updates download: the contracts
// it reported, to drop the portfolio items it did not report at its end.
type acctDownload struct {
	account string
	seen    map[int64]bool
}

// beginAcctDownload starts tracking the download of a (re)subscription.
func (ib *IB) beginAcctDownload(acct string) {
	ib.dlMu.Lock()
	ib.acctDL = &acctDownload{account: acct, seen: map[int64]bool{}}
	ib.dlMu.Unlock()
}

// noteAcctDownload records a portfolio row of the running download.
func (ib *IB) noteAcctDownload(acct string, conID int64) {
	ib.dlMu.Lock()
	if dl := ib.acctDL; dl != nil && dl.account == acct {
		dl.seen[conID] = true
	}
	ib.dlMu.Unlock()
}

// finishAcctDownload replaces the account's portfolio with the rows of the
// download that just ended: items it did not report are removed (and fire
// PortfolioEvent with a zero position).
func (ib *IB) finishAcctDownload(acct string) {
	ib.dlMu.Lock()
	dl := ib.acctDL
	if dl == nil || dl.account != acct {
		ib.dlMu.Unlock()
		return
	}
	ib.acctDL = nil
	ib.dlMu.Unlock()

	var gone []account.PortfolioItem
	ib.state.Mu.Lock()
	for conID, it := range ib.state.Portfolio[acct] {
		if !dl.seen[conID] {
			gone = append(gone, *it)
			delete(ib.state.Portfolio[acct], conID)
		}
	}
	ib.state.Mu.Unlock()
	sort.Slice(gone, func(i, j int) bool { return gone[i].Contract.ConID < gone[j].Contract.ConID })
	for i := range gone {
		gone[i].Position, gone[i].MarketValue = 0, 0
		ib.PortfolioEvent.Emit(&gone[i])
	}
}

// Portfolio returns the portfolio items received through ReqAccountUpdates
// (all accounts), sorted by account and contract id.
func (ib *IB) Portfolio() []account.PortfolioItem {
	ib.state.Mu.RLock()
	defer ib.state.Mu.RUnlock()
	var items []account.PortfolioItem
	for _, byCon := range ib.state.Portfolio {
		for _, it := range byCon {
			items = append(items, *it)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Account != items[j].Account {
			return items[i].Account < items[j].Account
		}
		return items[i].Contract.ConID < items[j].Contract.ConID
	})
	return items
}

func (ib *IB) handleAccountDownloadEnd(r *protocol.FieldReader) {
	r.Skip(1) // version
	ib.finishAcctDownload(r.ReadString())
	if key := ib.acctReq.active(); key != 0 {
		ib.state.EndReq(key, nil, nil)
	}
}

// --- PnL ---

type pnlKey struct {
	account, modelCode string
	conID              int64
}

// ReqPnL subscribes to the daily, unrealized and realized PnL of an account
// ("" = the only managed account). Updates (about once per second) fire
// PnLEvent; PnL() returns the latest values.
func (ib *IB) ReqPnL(acct, modelCode string) error {
	acct, err := ib.defaultAccount(acct)
	if err != nil {
		return err
	}
	key := pnlKey{account: acct, modelCode: modelCode}
	ib.pnlMu.Lock()
	if _, ok := ib.pnl[key]; ok {
		ib.pnlMu.Unlock()
		return nil // already subscribed
	}
	p := &account.PnL{Account: acct, ModelCode: modelCode, DailyPnL: math.NaN(), UnrealizedPnL: math.NaN(), RealizedPnL: math.NaN()}
	ib.pnl[key] = p
	ib.pnlMu.Unlock()
	_, err = ib.startSub(&subscription{
		kind: subPnL, target: key,
		send:   func(reqID int64) error { return ib.client.ReqPnL(reqID, acct, modelCode) },
		cancel: ib.client.CancelPnL,
	})
	if err != nil {
		ib.pnlMu.Lock()
		delete(ib.pnl, key)
		ib.pnlMu.Unlock()
	}
	return err
}

// CancelPnL cancels a ReqPnL subscription.
func (ib *IB) CancelPnL(acct, modelCode string) {
	acct, _ = ib.defaultAccount(acct)
	key := pnlKey{account: acct, modelCode: modelCode}
	ib.endSub(func(s *subscription) bool { return s.kind == subPnL && s.target == key })
	ib.pnlMu.Lock()
	delete(ib.pnl, key)
	ib.pnlMu.Unlock()
}

// PnL returns the latest values of all ReqPnL subscriptions.
func (ib *IB) PnL() []account.PnL {
	ib.pnlMu.Lock()
	defer ib.pnlMu.Unlock()
	out := make([]account.PnL, 0, len(ib.pnl))
	for _, p := range ib.pnl {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account+out[i].ModelCode < out[j].Account+out[j].ModelCode })
	return out
}

// ReqPnLSingle subscribes to the PnL of one position (conID) of an account
// ("" = the only managed account). Updates fire PnLSingleEvent;
// PnLSingles() returns the latest values.
func (ib *IB) ReqPnLSingle(acct, modelCode string, conID int64) error {
	acct, err := ib.defaultAccount(acct)
	if err != nil {
		return err
	}
	key := pnlKey{account: acct, modelCode: modelCode, conID: conID}
	ib.pnlMu.Lock()
	if _, ok := ib.pnlSingle[key]; ok {
		ib.pnlMu.Unlock()
		return nil
	}
	nan := math.NaN()
	p := &account.PnLSingle{Account: acct, ModelCode: modelCode, ConID: conID, DailyPnL: nan, UnrealizedPnL: nan, RealizedPnL: nan, Position: nan, Value: nan}
	ib.pnlSingle[key] = p
	ib.pnlMu.Unlock()
	_, err = ib.startSub(&subscription{
		kind: subPnLSingle, target: key,
		send:   func(reqID int64) error { return ib.client.ReqPnLSingle(reqID, acct, modelCode, conID) },
		cancel: ib.client.CancelPnLSingle,
	})
	if err != nil {
		ib.pnlMu.Lock()
		delete(ib.pnlSingle, key)
		ib.pnlMu.Unlock()
	}
	return err
}

// CancelPnLSingle cancels a ReqPnLSingle subscription.
func (ib *IB) CancelPnLSingle(acct, modelCode string, conID int64) {
	acct, _ = ib.defaultAccount(acct)
	key := pnlKey{account: acct, modelCode: modelCode, conID: conID}
	ib.endSub(func(s *subscription) bool { return s.kind == subPnLSingle && s.target == key })
	ib.pnlMu.Lock()
	delete(ib.pnlSingle, key)
	ib.pnlMu.Unlock()
}

// PnLSingles returns the latest values of all ReqPnLSingle subscriptions.
func (ib *IB) PnLSingles() []account.PnLSingle {
	ib.pnlMu.Lock()
	defer ib.pnlMu.Unlock()
	out := make([]account.PnLSingle, 0, len(ib.pnlSingle))
	for _, p := range ib.pnlSingle {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConID < out[j].ConID })
	return out
}

// handlePnL: reqId, dailyPnL, unrealizedPnL, realizedPnL.
func (ib *IB) handlePnL(r *protocol.FieldReader) {
	reqID := r.ReadInt()
	daily := unsetToNaN(r.ReadFloat())
	unrealized := unsetToNaN(r.ReadFloat())
	realized := unsetToNaN(r.ReadFloat())
	key, ok := ib.subscriberFor(reqID).(pnlKey)
	if !ok {
		return
	}
	ib.pnlMu.Lock()
	p := ib.pnl[key]
	if p == nil {
		ib.pnlMu.Unlock()
		return
	}
	p.DailyPnL, p.UnrealizedPnL, p.RealizedPnL = daily, unrealized, realized
	cp := *p
	ib.pnlMu.Unlock()
	ib.PnLEvent.Emit(&cp)
}

// handlePnLSingle: reqId, position, dailyPnL, unrealizedPnL, realizedPnL,
// value.
func (ib *IB) handlePnLSingle(r *protocol.FieldReader) {
	reqID := r.ReadInt()
	pos := unsetToNaN(r.ReadFloat())
	daily := unsetToNaN(r.ReadFloat())
	unrealized := unsetToNaN(r.ReadFloat())
	realized := unsetToNaN(r.ReadFloat())
	value := unsetToNaN(r.ReadFloat())
	key, ok := ib.subscriberFor(reqID).(pnlKey)
	if !ok {
		return
	}
	ib.pnlMu.Lock()
	p := ib.pnlSingle[key]
	if p == nil {
		ib.pnlMu.Unlock()
		return
	}
	p.Position, p.DailyPnL, p.UnrealizedPnL, p.RealizedPnL, p.Value = pos, daily, unrealized, realized, value
	cp := *p
	ib.pnlMu.Unlock()
	ib.PnLSingleEvent.Emit(&cp)
}

// --- Server time ---

// ReqCurrentTime returns the TWS/Gateway server time (second resolution).
func (ib *IB) ReqCurrentTime(ctx context.Context) (time.Time, error) {
	v, err := ib.runSingle(ctx, ib.timeReq, false, ib.client.ReqCurrentTime)
	if err != nil {
		return time.Time{}, err
	}
	sec, _ := v.(int64)
	return time.Unix(sec, 0).UTC(), nil
}

func (ib *IB) handleCurrentTime(r *protocol.FieldReader) {
	r.Skip(1) // version
	serverTime := r.ReadInt()
	ib.serverTime.Store(serverTime)
	if key := ib.timeReq.active(); key != 0 {
		ib.state.EndReq(key, serverTime, nil)
	}
}

// --- Contracts ---

// ErrContractNotFound is matched by QualifyContracts errors for contracts
// that matched no instrument.
var ErrContractNotFound = errors.New("ibgo: no contract found")

// ErrAmbiguousContract is matched by QualifyContracts errors for contracts
// that matched more than one instrument.
var ErrAmbiguousContract = errors.New("ibgo: ambiguous contract")

// QualifyError is the error of one contract in QualifyContracts.
type QualifyError struct {
	Contract *contract.Contract
	// Matches holds the candidates of an ambiguous contract.
	Matches []contract.ContractDetails
	Err     error
}

func (e *QualifyError) Error() string {
	if len(e.Matches) > 1 {
		return fmt.Sprintf("ibgo: ambiguous contract %s: %d matches (set exchange/currency/primaryExchange)", e.Contract, len(e.Matches))
	}
	return fmt.Sprintf("ibgo: cannot qualify %s: %v", e.Contract, e.Err)
}

func (e *QualifyError) Unwrap() error { return e.Err }

// QualifyContracts completes the given contracts in place (conId, local
// symbol, trading class, multiplier, ...) from their contract details, so
// they can be used for market data, orders and as map keys. Each contract
// must match exactly one instrument. Contracts that qualify are updated
// even when others fail; the error joins one *QualifyError per failure
// (errors.Is: ErrContractNotFound, ErrAmbiguousContract).
//
// An exchange of "SMART" and IncludeExpired are kept (TWS reports the
// listing exchange; IncludeExpired is a request option).
func (ib *IB) QualifyContracts(ctx context.Context, contracts ...*contract.Contract) error {
	errs := make([]error, len(contracts))
	found := make([]*contract.Contract, len(contracts))
	var wg sync.WaitGroup
	for i, c := range contracts {
		wg.Add(1)
		go func(i int, c *contract.Contract) {
			defer wg.Done()
			// Only reads c: the same pointer may be passed more than once.
			details, err := ib.ReqContractDetails(ctx, c)
			var ibe *IBError
			switch {
			case errors.As(err, &ibe) && ibe.Code == 200:
				errs[i] = &QualifyError{Contract: c, Err: fmt.Errorf("%w: %s", ErrContractNotFound, ibe.Message)}
			case err != nil:
				errs[i] = &QualifyError{Contract: c, Err: err}
			case len(details) == 0:
				errs[i] = &QualifyError{Contract: c, Err: ErrContractNotFound}
			case len(details) > 1:
				errs[i] = &QualifyError{Contract: c, Matches: details, Err: ErrAmbiguousContract}
			default:
				found[i] = details[0].Contract
			}
		}(i, c)
	}
	wg.Wait()
	// Update in place once all lookups are done.
	for i, c := range contracts {
		q := found[i]
		if q == nil {
			continue
		}
		exchange, includeExpired := c.Exchange, c.IncludeExpired
		*c = *q
		if exchange == "SMART" {
			c.Exchange = exchange
		}
		// A request option, not reported in the contract details.
		c.IncludeExpired = includeExpired
	}
	return errors.Join(errs...)
}

// ReqMatchingSymbols searches contracts by symbol or company name prefix.
func (ib *IB) ReqMatchingSymbols(ctx context.Context, pattern string) ([]contract.ContractDescription, error) {
	reqID := ib.client.GetReqID()
	ch := ib.state.StartReq(reqID, nil)
	if err := ib.client.ReqMatchingSymbols(reqID, pattern); err != nil {
		ib.state.CancelReq(reqID)
		return nil, err
	}
	v, err := ib.awaitReq(ctx, reqID, ch)
	if err != nil {
		return nil, err
	}
	descs, _ := v.([]contract.ContractDescription)
	return descs, nil
}

// handleSymbolSamples: reqId, n, n x (conId, symbol, secType,
// primaryExchange, currency, nDerivs, derivs..., [description, issuerId]).
func (ib *IB) handleSymbolSamples(r *protocol.FieldReader) {
	reqID := r.ReadInt()
	n := int(r.ReadInt())
	descs := make([]contract.ContractDescription, 0, n)
	for range n {
		c := &contract.Contract{
			ConID:           r.ReadInt(),
			Symbol:          r.ReadString(),
			SecType:         r.ReadString(),
			PrimaryExchange: r.ReadString(),
			Currency:        r.ReadString(),
		}
		nd := int(r.ReadInt())
		derivs := make([]string, 0, nd)
		for range nd {
			derivs = append(derivs, r.ReadString())
		}
		if ib.client.ServerVersion() >= 176 {
			c.Description = r.ReadString()
			c.IssuerID = r.ReadString()
		}
		descs = append(descs, contract.ContractDescription{Contract: c, DerivativeSecTypes: derivs})
	}
	ib.state.EndReq(reqID, descs, nil)
}

// ReqMarketRule returns the price increments of a market rule (see
// ContractDetails.MarketRuleIDs).
func (ib *IB) ReqMarketRule(ctx context.Context, marketRuleID int) ([]account.PriceIncrement, error) {
	key := ib.syntheticReqID.Add(-1)
	ch := ib.state.StartReq(key, nil)
	ib.ruleMu.Lock()
	ib.ruleWaiters[marketRuleID] = append(ib.ruleWaiters[marketRuleID], key)
	ib.ruleMu.Unlock()
	defer func() {
		ib.ruleMu.Lock()
		w := ib.ruleWaiters[marketRuleID]
		for i, k := range w {
			if k == key {
				w = append(w[:i], w[i+1:]...)
				break
			}
		}
		if len(w) == 0 {
			delete(ib.ruleWaiters, marketRuleID)
		} else {
			ib.ruleWaiters[marketRuleID] = w
		}
		ib.ruleMu.Unlock()
	}()
	if err := ib.client.ReqMarketRule(marketRuleID); err != nil {
		ib.state.CancelReq(key)
		return nil, err
	}
	v, err := ib.awaitReq(ctx, key, ch)
	if err != nil {
		return nil, err
	}
	incs, _ := v.([]account.PriceIncrement)
	return incs, nil
}

// handleMarketRule: marketRuleId, n, n x (lowEdge, increment).
func (ib *IB) handleMarketRule(r *protocol.FieldReader) {
	ruleID := int(r.ReadInt())
	n := int(r.ReadInt())
	incs := make([]account.PriceIncrement, 0, n)
	for range n {
		incs = append(incs, account.PriceIncrement{LowEdge: r.ReadFloat(), Increment: r.ReadFloat()})
	}
	ib.ruleMu.Lock()
	keys := ib.ruleWaiters[ruleID]
	delete(ib.ruleWaiters, ruleID)
	ib.ruleMu.Unlock()
	for _, k := range keys {
		ib.state.EndReq(k, incs, nil)
	}
}
