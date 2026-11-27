package ibgo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// ReconnectConfig configures automatic reconnection behavior.
type ReconnectConfig struct {
	// RetryDelay is the delay between reconnection attempts.
	RetryDelay time.Duration
	// MaxRetries is the number of retries after consecutive failed
	// connection attempts (MaxRetries+1 attempts in total). 0 = unlimited.
	MaxRetries int
	// ConnectTimeout bounds each connection attempt (default 10s).
	ConnectTimeout time.Duration
	// ProbeInterval is how often the connection is probed with
	// reqCurrentTime. If zero, no heartbeat probing is performed.
	ProbeInterval time.Duration
	// ProbeTimeout is how long to wait for any inbound message after a
	// probe before declaring the peer dead (default 4s).
	ProbeTimeout time.Duration
	// SyncOrders runs SyncOrders after every successful (re)connection,
	// before onConnect is called, so locally held Trade objects catch up on
	// fills and status changes that happened while disconnected.
	SyncOrders bool
	// SyncOptions are passed to SyncOrders when SyncOrders is set.
	SyncOptions SyncOptions
	// SyncTimeout bounds the post-connect sync (default 30s).
	SyncTimeout time.Duration
	// ClientIDFallbacks is how many following client ids (clientID+1,
	// clientID+2, ...) a connection attempt tries when TWS refuses the
	// client id because another API client is connected with it (error
	// 326), before the attempt counts as failed. ClientID reports the id in
	// use. The first connection starts at clientID. A reconnect starts at the
	// id it was last connected with and wraps around to clientID, so after a
	// TWS restart a client that had fallen back does not take clientID from
	// the client that held it. 0 (default) = none.
	// Leave it 0 for a client that places orders: TWS binds orders to the
	// client id that placed them, so after connecting with another id the
	// client could no longer modify or cancel its earlier orders, and
	// SyncOrders would not recognise them as its own.
	ClientIDFallbacks int
}

// connErrBox holds a connection error in an atomic.Pointer.
type connErrBox struct{ err error }

// LastConnectError returns why the last connection attempt of
// ConnectWithReconnect failed, or nil when the last attempt succeeded or
// none has been made. Use errors.Is(err, ErrClientIDInUse) to detect a
// client id that another API client holds.
func (ib *IB) LastConnectError() error {
	if b := ib.connErr.Load(); b != nil {
		return b.err
	}
	return nil
}

// connectAnyClientID connects with one of the ids clientID ..
// clientID+cfg.ClientIDFallbacks, moving to the next while TWS refuses one as
// already in use. It starts at start and wraps around past the last id.
func (ib *IB) connectAnyClientID(ctx context.Context, host string, port, clientID, start int, cfg ReconnectConfig, timeout time.Duration) error {
	fallbacks := max(cfg.ClientIDFallbacks, 0)
	n := fallbacks + 1
	offset := start - clientID
	if offset < 0 || offset >= n {
		offset = 0
	}
	idAt := func(i int) int { return clientID + (offset+i)%n }
	var err error
	for i := 0; i < n; i++ {
		connectCtx, cancel := context.WithTimeout(ctx, timeout)
		err = ib.Connect(connectCtx, host, port, idAt(i))
		cancel()
		if err == nil || !errors.Is(err, ErrClientIDInUse) || ctx.Err() != nil {
			return err
		}
		if i < n-1 {
			log.Printf("ibgo: client id %d is in use, trying %d", idAt(i), idAt(i+1))
		}
	}
	if fallbacks > 0 {
		return fmt.Errorf("client ids %d-%d are all in use: %w", clientID, clientID+fallbacks, err)
	}
	return err
}

// DefaultReconnectConfig returns sensible defaults for reconnection.
func DefaultReconnectConfig() ReconnectConfig {
	return ReconnectConfig{
		RetryDelay:     2 * time.Second,
		MaxRetries:     0, // unlimited
		ConnectTimeout: 10 * time.Second,
		ProbeInterval:  30 * time.Second,
		ProbeTimeout:   4 * time.Second,
		SyncOrders:     true,
		SyncTimeout:    30 * time.Second,
	}
}

// ConnectWithReconnect connects to TWS/Gateway and automatically reconnects
// on disconnection. Blocks until ctx is cancelled (returns ctx.Err()) or
// MaxRetries consecutive attempts fail (returns the last error).
//
// Unlike Disconnect, an automatic reconnect keeps orders, fills and positions
// so Trade objects held by the application stay valid. Streaming
// subscriptions (ReqMktData, ReqTickByTickData, ReqMktDepth,
// ReqRealTimeBars, ReqHistoricalDataStream, ReqPnL, ReqPnLSingle,
// ReqAccountUpdates) and the market data type are re-requested
// automatically, so the Ticker / bar list objects keep updating. The
// market data type is re-applied before ConnectedEvent fires; the
// subscriptions after the order sync. When the account updates download
// completes, positions closed while disconnected leave Portfolio().
// onConnect is called after each successful (re)connection (and sync).
func (ib *IB) ConnectWithReconnect(ctx context.Context, host string, port, clientID int, cfg ReconnectConfig, onConnect func()) error {
	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	attempt := 0
	// The id of the last connection: a reconnect tries it first.
	lastID := clientID

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := ib.connectAnyClientID(ctx, host, port, clientID, lastID, cfg, connectTimeout)

		if err != nil {
			ib.connErr.Store(&connErrBox{err: err})
			attempt++
			if cfg.MaxRetries > 0 && attempt > cfg.MaxRetries {
				return err
			}
			log.Printf("ibgo: connection failed (attempt %d): %v, retrying in %v", attempt, err, cfg.RetryDelay)
			select {
			case <-time.After(cfg.RetryDelay):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		// Connected successfully
		ib.connErr.Store(nil)
		lastID = int(ib.ClientID())
		attempt = 0
		done := ib.client.Done()
		log.Printf("ibgo: connected to %s:%d as client %d", host, port, ib.ClientID())

		// Start heartbeat if configured
		var probeDone chan struct{}
		if cfg.ProbeInterval > 0 {
			probeDone = make(chan struct{})
			go ib.heartbeatLoop(ctx, cfg, done, probeDone)
		}

		if cfg.SyncOrders {
			syncTimeout := cfg.SyncTimeout
			if syncTimeout <= 0 {
				syncTimeout = 30 * time.Second
			}
			syncCtx, cancelSync := context.WithTimeout(ctx, syncTimeout)
			if _, err := ib.SyncOrders(syncCtx, cfg.SyncOptions); err != nil {
				log.Printf("ibgo: order sync after connect failed: %v", err)
			}
			cancelSync()
		}

		ib.restoreSubscriptions()

		if onConnect != nil {
			onConnect()
		}

		// Wait for disconnect or context cancel
		select {
		case <-done:
			log.Printf("ibgo: disconnected, will reconnect in %v", cfg.RetryDelay)
		case <-ctx.Done():
			ib.Disconnect()
			if probeDone != nil {
				<-probeDone
			}
			return ctx.Err()
		}

		if probeDone != nil {
			<-probeDone
		}

		// Drop per-connection state but keep orders/fills/positions.
		ib.state.Requests.DrainAll(ErrDisconnected)
		ib.resetSession()

		select {
		case <-time.After(cfg.RetryDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// heartbeatLoop periodically probes the connection to detect dead peers.
// It sends reqCurrentTime and requires *some* inbound message within
// ProbeTimeout. On timeout it drops the socket (not the state), which makes
// ConnectWithReconnect reconnect.
func (ib *IB) heartbeatLoop(ctx context.Context, cfg ReconnectConfig, connDone <-chan struct{}, done chan struct{}) {
	defer close(done)

	probeTimeout := cfg.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = 4 * time.Second
	}

	ticker := time.NewTicker(cfg.ProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if !ib.client.IsConnected() {
				return
			}

			sent := time.Now()
			if err := ib.client.ReqCurrentTime(); err != nil {
				log.Printf("ibgo: heartbeat write failed: %v", err)
				ib.client.Disconnect()
				return
			}

			timer := time.NewTimer(probeTimeout)
			select {
			case <-timer.C:
				if ib.client.Conn.LastRecv().Before(sent) {
					log.Printf("ibgo: heartbeat timeout — peer not responding, disconnecting")
					ib.client.Disconnect()
					return
				}
			case <-connDone:
				timer.Stop()
				return
			case <-ctx.Done():
				timer.Stop()
				return
			}
		case <-connDone:
			return
		case <-ctx.Done():
			return
		}
	}
}
