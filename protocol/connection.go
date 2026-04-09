// Package protocol implements the IB TWS wire protocol.
package protocol

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Connection manages a TCP connection to TWS/Gateway with IB message framing.
type Connection struct {
	conn   net.Conn
	reader *bufio.Reader // buffered reader for batch detection
	mu     sync.Mutex    // protects writes

	NumBytesSent atomic.Int64
	NumMsgSent   atomic.Int64
	NumBytesRecv atomic.Int64
	NumMsgRecv   atomic.Int64
}

// Connect establishes a TCP connection to host:port.
func (c *Connection) Connect(host string, port int) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	c.conn = conn
	c.reader = bufio.NewReaderSize(conn, 64*1024)
	return nil
}

// Disconnect closes the TCP connection.
// The conn.Close() unblocks any pending ReadMessage call.
func (c *Connection) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.reader = nil
	return err
}

// IsConnected returns true if a TCP connection exists.
func (c *Connection) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Conn returns the underlying net.Conn (for setting deadlines, etc.).
func (c *Connection) Conn() net.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

// SendRaw writes raw bytes to the socket (used for the initial handshake).
func (c *Connection) SendRaw(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	_, err := c.conn.Write(data)
	if err != nil {
		return err
	}
	c.NumBytesSent.Add(int64(len(data)))
	return nil
}

// SendMessage writes a length-prefixed message: [4-byte big-endian len][payload].
func (c *Connection) SendMessage(payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	_, err := c.conn.Write(header)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(payload)
	if err != nil {
		return err
	}
	c.NumBytesSent.Add(int64(4 + len(payload)))
	c.NumMsgSent.Add(1)
	return nil
}

// ReadMessage reads one length-prefixed message and splits on null bytes.
// Returns the fields (without trailing empty string).
func (c *Connection) ReadMessage() ([]string, error) {
	c.mu.Lock()
	r := c.reader
	c.mu.Unlock()
	if r == nil {
		return nil, fmt.Errorf("not connected")
	}

	// Read 4-byte length prefix
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	msgLen := binary.BigEndian.Uint32(header)
	if msgLen == 0 {
		return nil, fmt.Errorf("zero-length message")
	}
	if msgLen > 10*1024*1024 { // 10MB sanity limit
		return nil, fmt.Errorf("message too large: %d bytes", msgLen)
	}

	// Read message body
	body := make([]byte, msgLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}

	c.NumBytesRecv.Add(int64(4 + msgLen))
	c.NumMsgRecv.Add(1)

	// Split on null bytes, remove trailing empty element
	msg := string(body)
	fields := strings.Split(msg, "\x00")
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}

	return fields, nil
}

// HasPending returns true if the buffered reader has unprocessed data,
// meaning more IB messages arrived in the same network read batch.
func (c *Connection) HasPending() bool {
	c.mu.Lock()
	r := c.reader
	c.mu.Unlock()
	return r != nil && r.Buffered() > 0
}
