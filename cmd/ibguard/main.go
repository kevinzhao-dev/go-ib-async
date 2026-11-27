// ibguard is a TCP proxy between API clients and a TWS/IB Gateway that
// guarantees no order can reach the gateway: it parses every client->gateway
// message and kills the session, without forwarding the message, when it is
// placeOrder, cancelOrder, reqGlobalCancel, exerciseOptions, replaceFA or
// reqAutoOpenOrders. Everything else (market data, account, historical data,
// contract details, ...) passes through.
//
// It fails closed: a message whose id is not a plain decimal id between 1
// and maxMsgID also kills the session. The API version offered by the
// client is capped at maxServerVersion, because newer servers send binary
// message ids and protobuf messages the guard cannot inspect.
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

const (
	// maxServerVersion is the highest API version negotiated through the
	// guard. Up to it every message starts with its id as decimal text.
	maxServerVersion = 178
	// maxMsgID is the highest outbound message id up to maxServerVersion
	// (REQ_USER_INFO); higher ids are unknown and rejected.
	maxMsgID = 104
)

// checkOutbound returns why a client message must not be forwarded, or ""
// when it may pass. The id is parsed as TWS parses it (a decimal number),
// so encodings like "03" cannot sneak past the block list.
func checkOutbound(p []byte) string {
	i := bytes.IndexByte(p, 0)
	if i <= 0 {
		return "message without a text id"
	}
	s := string(p[:i])
	for _, c := range s {
		if c < '0' || c > '9' {
			return fmt.Sprintf("non-numeric message id %q", s)
		}
	}
	id, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Sprintf("bad message id %q", s)
	}
	if name, bad := blocked[id]; bad {
		return fmt.Sprintf("%s (%d)", name, id)
	}
	if id < 1 || id > maxMsgID {
		return fmt.Sprintf("unknown message id %d", id)
	}
	return ""
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
	for {
		p, err := readFrame(r)
		if err != nil {
			kill()
			log.Printf("c%d: session closed", id)
			return
		}
		if why := checkOutbound(p); why != "" {
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
	log.Printf("guard listening on %s -> %s (pid %d, blocking %d order message types)", *listen, *upstream, os.Getpid(), len(blocked))
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
