// ibguard is a TCP proxy between API clients and a TWS/IB Gateway that
// guarantees no order can reach the gateway: it parses every client->gateway
// message and kills the session, without forwarding the message, when it is
// placeOrder, cancelOrder, reqGlobalCancel, exerciseOptions, replaceFA,
// reqAutoOpenOrders, or reqOpenOrders from client id 0 (which binds manual
// TWS orders to the API). Everything else (market data, account, historical
// data, contract details, ...) passes through.
//
// It fails closed: only message ids on an explicit allowlist pass, so a
// message whose id is unassigned or not a plain decimal number also kills
// the session. The API version offered by the client is capped at
// maxServerVersion, because newer servers send binary message ids and
// protobuf messages the guard cannot inspect.
//
// Use it to develop or test read-only features against a live account:
//
//	go run ./cmd/ibguard -upstream 127.0.0.1:4001 -listen 127.0.0.1:4011
//	IB_PORT=4011 go run ./cmd/smoketest
//
// Optionally it dumps all traffic (both directions, fields separated by
// "|") to -dump, which is handy for checking decoders against real
// messages; the dump contains account data, so keep it private.
// Sending SIGUSR1 drops all sessions (to test reconnect handling). Signal
// the pid the guard logs at startup: under "go run", $! is the pid of the
// go command, which does not forward SIGUSR1.
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// live sessions, closed on SIGUSR1 to simulate a dropped connection.
var (
	liveMu sync.Mutex
	live   = map[int]func(){}
)

// Outbound message ids that must never reach a live gateway.
var blocked = map[int]string{
	3:  "PLACE_ORDER",
	4:  "CANCEL_ORDER",
	15: "REQ_AUTO_OPEN_ORDERS",
	19: "REPLACE_FA",
	21: "EXERCISE_OPTIONS",
	58: "REQ_GLOBAL_CANCEL",
}

// allowed lists every outbound message id that may reach the gateway (up to
// maxServerVersion). Ids not listed, including unassigned ones, kill the
// session. REQ_OPEN_ORDERS is further restricted, see session.check.
var allowed = map[int]string{
	1: "REQ_MKT_DATA", 2: "CANCEL_MKT_DATA", 5: "REQ_OPEN_ORDERS",
	6: "REQ_ACCOUNT_UPDATES", 7: "REQ_EXECUTIONS", 8: "REQ_IDS",
	9: "REQ_CONTRACT_DETAILS", 10: "REQ_MKT_DEPTH", 11: "CANCEL_MKT_DEPTH",
	12: "REQ_NEWS_BULLETINS", 13: "CANCEL_NEWS_BULLETINS",
	14: "SET_SERVER_LOGLEVEL", 16: "REQ_ALL_OPEN_ORDERS",
	17: "REQ_MANAGED_ACCTS", 18: "REQ_FA", 20: "REQ_HISTORICAL_DATA",
	22: "REQ_SCANNER_SUBSCRIPTION", 23: "CANCEL_SCANNER_SUBSCRIPTION",
	24: "REQ_SCANNER_PARAMETERS", 25: "CANCEL_HISTORICAL_DATA",
	49: "REQ_CURRENT_TIME", 50: "REQ_REAL_TIME_BARS", 51: "CANCEL_REAL_TIME_BARS",
	52: "REQ_FUNDAMENTAL_DATA", 53: "CANCEL_FUNDAMENTAL_DATA",
	54: "REQ_CALC_IMPLIED_VOLAT", 55: "REQ_CALC_OPTION_PRICE",
	56: "CANCEL_CALC_IMPLIED_VOLAT", 57: "CANCEL_CALC_OPTION_PRICE",
	59: "REQ_MARKET_DATA_TYPE", 61: "REQ_POSITIONS", 62: "REQ_ACCOUNT_SUMMARY",
	63: "CANCEL_ACCOUNT_SUMMARY", 64: "CANCEL_POSITIONS",
	65: "VERIFY_REQUEST", 66: "VERIFY_MESSAGE", 67: "QUERY_DISPLAY_GROUPS",
	68: "SUBSCRIBE_TO_GROUP_EVENTS", 69: "UPDATE_DISPLAY_GROUP",
	70: "UNSUBSCRIBE_FROM_GROUP_EVENTS", 71: "START_API",
	72: "VERIFY_AND_AUTH_REQUEST", 73: "VERIFY_AND_AUTH_MESSAGE",
	74: "REQ_POSITIONS_MULTI", 75: "CANCEL_POSITIONS_MULTI",
	76: "REQ_ACCOUNT_UPDATES_MULTI", 77: "CANCEL_ACCOUNT_UPDATES_MULTI",
	78: "REQ_SEC_DEF_OPT_PARAMS", 79: "REQ_SOFT_DOLLAR_TIERS",
	80: "REQ_FAMILY_CODES", 81: "REQ_MATCHING_SYMBOLS",
	82: "REQ_MKT_DEPTH_EXCHANGES", 83: "REQ_SMART_COMPONENTS",
	84: "REQ_NEWS_ARTICLE", 85: "REQ_NEWS_PROVIDERS", 86: "REQ_HISTORICAL_NEWS",
	87: "REQ_HEAD_TIMESTAMP", 88: "REQ_HISTOGRAM_DATA", 89: "CANCEL_HISTOGRAM_DATA",
	90: "CANCEL_HEAD_TIMESTAMP", 91: "REQ_MARKET_RULE", 92: "REQ_PNL",
	93: "CANCEL_PNL", 94: "REQ_PNL_SINGLE", 95: "CANCEL_PNL_SINGLE",
	96: "REQ_HISTORICAL_TICKS", 97: "REQ_TICK_BY_TICK_DATA",
	98: "CANCEL_TICK_BY_TICK_DATA", 99: "REQ_COMPLETED_ORDERS",
	100: "REQ_WSH_META_DATA", 101: "CANCEL_WSH_META_DATA",
	102: "REQ_WSH_EVENT_DATA", 103: "CANCEL_WSH_EVENT_DATA", 104: "REQ_USER_INFO",
}

const (
	// maxServerVersion is the highest API version negotiated through the
	// guard. Up to it every message starts with its id as decimal text.
	maxServerVersion = 178

	msgReqOpenOrders = 5
	msgStartAPI      = 71
)

// session holds what the guard learned about one client session.
type session struct {
	// clientID is the API client id sent with START_API, -1 before.
	clientID int
}

func newSession() *session { return &session{clientID: -1} }

// check returns why a client message must not be forwarded, or "" when it
// may pass. The id is parsed as TWS parses it (a decimal number), so
// encodings like "03" cannot sneak past the lists.
//
// REQ_OPEN_ORDERS from client id 0 (or before START_API) is blocked: for
// client 0 TWS binds manually placed orders to the API, which can cancel
// and resubmit a working order and cost its queue priority.
// REQ_ALL_OPEN_ORDERS reads the same orders without binding them.
func (s *session) check(p []byte) string {
	fields := bytes.Split(p, []byte{0})
	if len(fields) < 2 || len(fields[0]) == 0 {
		return "message without a text id"
	}
	id, why := parseID(fields[0])
	if why != "" {
		return why
	}
	if name, bad := blocked[id]; bad {
		return fmt.Sprintf("%s (%d)", name, id)
	}
	name, ok := allowed[id]
	if !ok {
		return fmt.Sprintf("unknown message id %d", id)
	}
	switch id {
	case msgStartAPI:
		// START_API: id, version, clientId, optional capabilities.
		if s.clientID >= 0 {
			return "second START_API"
		}
		if len(fields) < 4 {
			return "START_API without a client id"
		}
		cid, why := parseID(fields[2])
		if why != "" {
			return "START_API client id: " + why
		}
		s.clientID = cid
	case msgReqOpenOrders:
		if s.clientID <= 0 {
			return fmt.Sprintf("%s (%d) from client id 0 or before START_API (binds manual TWS orders; use REQ_ALL_OPEN_ORDERS)", name, id)
		}
	}
	return ""
}

// parseID parses a plain non-negative decimal number.
func parseID(b []byte) (int, string) {
	if len(b) == 0 || len(b) > 9 {
		return 0, fmt.Sprintf("bad message id %q", b)
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, fmt.Sprintf("non-numeric message id %q", b)
		}
	}
	n, err := strconv.Atoi(string(b))
	if err != nil {
		return 0, fmt.Sprintf("bad message id %q", b)
	}
	return n, ""
}

// capHandshake rewrites the client's version range "vMIN..MAX[ options]"
// so MAX does not exceed maxServerVersion. It fails when the range cannot
// be parsed or starts above maxServerVersion.
func capHandshake(p []byte) ([]byte, error) {
	spec, opts, hasOpts := strings.Cut(string(p), " ")
	rest, ok := strings.CutPrefix(spec, "v")
	if !ok {
		return nil, fmt.Errorf("unexpected version string %q", p)
	}
	lo, hi, isRange := strings.Cut(rest, "..")
	if !isRange {
		hi = lo
	}
	minV, err1 := strconv.Atoi(lo)
	maxV, err2 := strconv.Atoi(hi)
	if err1 != nil || err2 != nil || minV < 1 || minV > maxV {
		return nil, fmt.Errorf("unexpected version string %q", p)
	}
	if minV > maxServerVersion {
		return nil, fmt.Errorf("client requires API version %d+, guard supports up to %d", minV, maxServerVersion)
	}
	out := spec
	if maxV > maxServerVersion {
		if !isRange {
			return nil, fmt.Errorf("client requires API version %d, guard supports up to %d", maxV, maxServerVersion)
		}
		out = fmt.Sprintf("v%d..%d", minV, maxServerVersion)
	}
	if hasOpts {
		out += " " + opts
	}
	return []byte(out), nil
}

var (
	listen   = flag.String("listen", "127.0.0.1:4011", "listen address")
	upstream = flag.String("upstream", "127.0.0.1:4001", "gateway address")
	dumpPath = flag.String("dump", "", "traffic dump file (empty: no dump)")
)

var (
	dumpMu sync.Mutex
	dump   *os.File
)

func record(conn int, dir string, payload []byte) {
	if dump == nil {
		return
	}
	fields := strings.Split(string(payload), "\x00")
	if n := len(fields); n > 0 && fields[n-1] == "" {
		fields = fields[:n-1]
	}
	dumpMu.Lock()
	fmt.Fprintf(dump, "%s c%d %s %s\n", time.Now().Format("15:04:05.000"), conn, dir, strings.Join(fields, "|"))
	dumpMu.Unlock()
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > 64<<20 {
		return nil, fmt.Errorf("frame too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeFrame(w io.Writer, payload []byte) error {
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	copy(frame[4:], payload)
	_, err := w.Write(frame)
	return err
}

func handle(id int, client net.Conn) {
	defer client.Close()
	server, err := net.Dial("tcp", *upstream)
	if err != nil {
		log.Printf("c%d: dial upstream: %v", id, err)
		return
	}
	defer server.Close()
	log.Printf("c%d: session opened", id)

	var once sync.Once
	kill := func() { once.Do(func() { client.Close(); server.Close() }) }
	liveMu.Lock()
	live[id] = kill
	liveMu.Unlock()
	defer func() { liveMu.Lock(); delete(live, id); liveMu.Unlock() }()

	// gateway -> client
	go func() {
		defer kill()
		r := bufio.NewReader(server)
		for {
			p, err := readFrame(r)
			if err != nil {
				return
			}
			record(id, "<-", p)
			if err := writeFrame(client, p); err != nil {
				return
			}
		}
	}()

	// client -> gateway
	r := bufio.NewReader(client)
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return
	}
	if string(magic[:]) != "API\x00" {
		log.Printf("c%d: unexpected handshake prefix %q, closing", id, magic)
		kill()
		return
	}
	ver, err := readFrame(r)
	if err != nil {
		return
	}
	record(id, "->", append([]byte("HANDSHAKE "), ver...))
	capped, err := capHandshake(ver)
	if err != nil {
		log.Printf("c%d: %v, closing", id, err)
		kill()
		return
	}
	if !bytes.Equal(capped, ver) {
		log.Printf("c%d: API version range %q capped to %q", id, ver, capped)
	}
	if _, err := server.Write(magic[:]); err != nil {
		return
	}
	if err := writeFrame(server, capped); err != nil {
		return
	}
	sess := newSession()
	for {
		p, err := readFrame(r)
		if err != nil {
			kill()
			log.Printf("c%d: session closed", id)
			return
		}
		if why := sess.check(p); why != "" {
			record(id, "XX", p)
			log.Printf("c%d: BLOCKED outbound %s - session killed, nothing forwarded", id, why)
			kill()
			return
		}
		record(id, "->", p)
		if err := writeFrame(server, p); err != nil {
			kill()
			return
		}
	}
}

func main() {
	flag.Parse()
	if *dumpPath != "" {
		f, err := os.OpenFile(*dumpPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			log.Fatal(err)
		}
		dump = f
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("guard listening on %s -> %s (pid %d, %d message types allowed, %d order message types blocked)", *listen, *upstream, os.Getpid(), len(allowed), len(blocked))
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1)
	go func() {
		for range sig {
			liveMu.Lock()
			for id, k := range live {
				log.Printf("c%d: dropped by SIGUSR1", id)
				k()
			}
			liveMu.Unlock()
		}
	}()
	for id := 1; ; id++ {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go handle(id, c)
	}
}
