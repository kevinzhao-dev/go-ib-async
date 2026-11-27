package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime/debug"
	"sync"
)

// The MCP stdio transport: one JSON-RPC 2.0 message per line on stdin and
// stdout. Only the server side of the protocol is implemented (tools; no
// resources, prompts or server-initiated requests).

// Protocol versions this server speaks, newest first.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type cancelledParams struct {
	RequestID json.RawMessage `json:"requestId"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callResult struct {
	Content           []textContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

// Serve runs the MCP protocol over r (requests) and w (responses) until r
// reaches EOF or ctx ends. Requests are handled concurrently; responses are
// written one per line. In-flight tool calls are cancelled when ctx ends and
// waited for before Serve returns.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sess := &session{srv: s, w: w, inflight: map[string]context.CancelFunc{}}
	defer sess.wg.Wait()

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					readErr <- ctx.Err()
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				readErr <- err
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case line := <-lines:
			sess.handleLine(ctx, line)
		}
	}
}

type session struct {
	srv *Server
	wg  sync.WaitGroup

	writeMu sync.Mutex
	w       io.Writer

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
}

func (ss *session) write(resp rpcResponse) {
	resp.JSONRPC = "2.0"
	if resp.ID == nil {
		resp.ID = json.RawMessage("null")
	}
	b, err := json.Marshal(resp)
	if err != nil {
		b, _ = json.Marshal(rpcResponse{JSONRPC: "2.0", ID: resp.ID, Error: &rpcError{codeInternalError, "cannot encode response: " + err.Error()}})
	}
	ss.writeMu.Lock()
	defer ss.writeMu.Unlock()
	if _, err := ss.w.Write(append(b, '\n')); err != nil {
		log.Printf("mcpserver: write response: %v", err)
	}
}

func (ss *session) writeError(id json.RawMessage, code int, msg string) {
	ss.write(rpcResponse{ID: id, Error: &rpcError{code, msg}})
}

func (ss *session) handleLine(ctx context.Context, line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) > 0 && line[0] == '[' {
		ss.writeError(nil, codeInvalidRequest, "JSON-RPC batches are not supported")
		return
	}
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		ss.writeError(nil, codeParseError, "parse error: "+err.Error())
		return
	}
	if msg.Method == "" {
		// A response to a server-initiated request; this server sends none.
		return
	}
	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		ss.handleNotification(msg)
		return
	}

	key := string(msg.ID)
	reqCtx, cancel := context.WithCancel(ctx)
	ss.mu.Lock()
	ss.inflight[key] = cancel
	ss.mu.Unlock()

	ss.wg.Add(1)
	go func() {
		defer ss.wg.Done()
		defer func() {
			ss.mu.Lock()
			delete(ss.inflight, key)
			ss.mu.Unlock()
			cancel()
		}()
		result, rerr := ss.dispatch(reqCtx, msg)
		if reqCtx.Err() != nil && ctx.Err() == nil {
			return // cancelled by the client: no response
		}
		if rerr != nil {
			ss.write(rpcResponse{ID: msg.ID, Error: rerr})
			return
		}
		ss.write(rpcResponse{ID: msg.ID, Result: result})
	}()
}

func (ss *session) handleNotification(msg rpcMessage) {
	switch msg.Method {
	case "notifications/cancelled":
		var p cancelledParams
		if json.Unmarshal(msg.Params, &p) == nil && len(p.RequestID) > 0 {
			ss.mu.Lock()
			cancel := ss.inflight[string(p.RequestID)]
			ss.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	default:
		// notifications/initialized and anything else: nothing to do.
	}
}

func (ss *session) dispatch(ctx context.Context, msg rpcMessage) (result any, rerr *rpcError) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("mcpserver: panic in %s: %v\n%s", msg.Method, p, debug.Stack())
			result, rerr = nil, &rpcError{codeInternalError, fmt.Sprintf("internal error: %v", p)}
		}
	}()
	switch msg.Method {
	case "initialize":
		var p initializeParams
		if len(msg.Params) > 0 {
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				return nil, &rpcError{codeInvalidParams, "invalid initialize params: " + err.Error()}
			}
		}
		return ss.srv.initializeResult(p.ProtocolVersion), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": ss.srv.tools}, nil
	case "tools/call":
		var p callParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, &rpcError{codeInvalidParams, "invalid tools/call params: " + err.Error()}
		}
		t := ss.srv.toolByName[p.Name]
		if t == nil {
			return nil, &rpcError{codeInvalidParams, fmt.Sprintf("unknown tool %q", p.Name)}
		}
		return ss.srv.callTool(ctx, t, p.Arguments), nil
	default:
		return nil, &rpcError{codeMethodNotFound, fmt.Sprintf("method %q not found", msg.Method)}
	}
}

func (s *Server) initializeResult(clientVersion string) map[string]any {
	version := protocolVersions[0]
	for _, v := range protocolVersions {
		if v == clientVersion {
			version = v
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    s.cfg.Name,
			"version": s.cfg.Version,
		},
		"instructions": s.instructions(),
	}
}

// callTool runs a tool and converts its outcome into a tools/call result.
// Failures are reported as tool errors (isError) so the model can read and
// correct them, rather than as protocol errors.
func (s *Server) callTool(ctx context.Context, t *tool, args json.RawMessage) (res callResult) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("mcpserver: panic in tool %s: %v\n%s", t.Name, p, debug.Stack())
			res = errorResult(fmt.Errorf("internal error: %v", p))
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	out, err := t.handler(ctx, args)
	if err != nil {
		return errorResult(err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return errorResult(fmt.Errorf("cannot encode result: %w", err))
	}
	return callResult{
		Content:           []textContent{{Type: "text", Text: string(b)}},
		StructuredContent: json.RawMessage(b),
	}
}

func errorResult(err error) callResult {
	return callResult{Content: []textContent{{Type: "text", Text: err.Error()}}, IsError: true}
}
