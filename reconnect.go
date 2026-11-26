package ibgo

import (
	"context"
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
// so Trade objects held by the application stay valid; market data
// subscriptions are dropped and must be re-requested in onConnect.
// onConnect is called after each successful (re)connection (and sync).
func (ib *IB) ConnectWithReconnect(ctx context.Context, host string, port, clientID int, cfg ReconnectConfig, onConnect func()) error {
	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	attempt := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		err := ib.Connect(connectCtx, host, port, clientID)
		cancel()

		if err != nil {
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
		attempt = 0
		done := ib.client.Done()
		log.Printf("ibgo: connected to %s:%d", host, port)

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
		ib.state.ResetSession()

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
