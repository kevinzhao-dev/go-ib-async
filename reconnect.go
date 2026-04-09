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
	// MaxRetries is the maximum number of reconnection attempts. 0 = unlimited.
	MaxRetries int
	// ProbeInterval is how long to wait without traffic before probing.
	// If zero, no heartbeat probing is performed.
	ProbeInterval time.Duration
	// ProbeTimeout is the timeout for each heartbeat probe.
	ProbeTimeout time.Duration
}

// DefaultReconnectConfig returns sensible defaults for reconnection.
func DefaultReconnectConfig() ReconnectConfig {
	return ReconnectConfig{
		RetryDelay:    2 * time.Second,
		MaxRetries:    0, // unlimited
		ProbeInterval: 30 * time.Second,
		ProbeTimeout:  4 * time.Second,
	}
}

// ConnectWithReconnect connects to TWS/Gateway and automatically reconnects
// on disconnection. Blocks until ctx is cancelled.
// onConnect is called after each successful (re)connection.
func (ib *IB) ConnectWithReconnect(ctx context.Context, host string, port, clientID int, cfg ReconnectConfig, onConnect func()) error {
	attempt := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
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
		log.Printf("ibgo: connected to %s:%d", host, port)

		if onConnect != nil {
			onConnect()
		}

		// Start heartbeat if configured
		var probeDone chan struct{}
		if cfg.ProbeInterval > 0 {
			probeDone = make(chan struct{})
			go ib.heartbeatLoop(ctx, cfg, probeDone)
		}

		// Wait for disconnect or context cancel
		select {
		case <-ib.client.Done():
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

		// Clean up state before reconnect
		ib.state.Requests.DrainAll(ErrDisconnected)
		ib.state.Reset()

		select {
		case <-time.After(cfg.RetryDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// heartbeatLoop periodically sends reqCurrentTime to detect dead connections.
func (ib *IB) heartbeatLoop(ctx context.Context, cfg ReconnectConfig, done chan struct{}) {
	defer close(done)

	ticker := time.NewTicker(cfg.ProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if !ib.client.IsConnected() {
				return
			}
			err := ib.client.ReqCurrentTime()
			if err != nil {
				log.Printf("ibgo: heartbeat failed: %v", err)
				ib.Disconnect()
				return
			}
		case <-ib.client.Done():
			return
		case <-ctx.Done():
			return
		}
	}
}
