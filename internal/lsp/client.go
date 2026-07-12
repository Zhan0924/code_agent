package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Client manages LSP server connections and provides semantic code operations.
type Client interface {
	// Initialize starts an LSP server for the given language and workspace.
	Initialize(ctx context.Context, language, rootPath string) error
	// Shutdown stops the LSP server for a language.
	Shutdown(language string) error
	// GotoDefinition finds the definition of a symbol.
	GotoDefinition(ctx context.Context, uri string, line, col int) ([]Location, error)
	// FindReferences finds all references to a symbol.
	FindReferences(ctx context.Context, uri string, line, col int) ([]Location, error)
	// Rename performs a semantic rename across the workspace.
	Rename(ctx context.Context, uri string, line, col int, newName string) (*WorkspaceEdit, error)
	// Hover gets hover information for a symbol.
	Hover(ctx context.Context, uri string, line, col int) (*HoverResult, error)
	// DocumentSymbols gets all symbols in a document.
	DocumentSymbols(ctx context.Context, uri string) ([]SymbolInfo, error)
	// DidChange notifies the server of document changes.
	DidChange(ctx context.Context, uri, content string) error
	// ShutdownAll stops all LSP servers.
	ShutdownAll() error
}

// Config holds LSP client configuration.
type Config struct {
	Servers map[string]ServerConfig
	Timeout int // seconds
}

// ServerConfig defines an LSP server for a language.
type ServerConfig struct {
	Command   string
	Args      []string
	Languages []string
}

// jsonrpcRequest is a JSON-RPC 2.0 request/notification frame.
type jsonrpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int64      `json:"id,omitempty"` // nil => notification
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// jsonrpcResponse is a JSON-RPC 2.0 response frame.
type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"` // server-initiated request/notification
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonrpcError) Error() string {
	return fmt.Sprintf("lsp rpc error %d: %s", e.Code, e.Message)
}

type client struct {
	cfg     Config
	logger  *zap.Logger
	mu      sync.RWMutex
	servers map[string]*serverConn // language -> connection
}

// NewClient creates a new LSP client.
func NewClient(cfg Config, logger *zap.Logger) Client {
	return &client{
		cfg:     cfg,
		logger:  logger.With(zap.String("component", "lsp-client")),
		servers: make(map[string]*serverConn),
	}
}

// serverConn owns a single LSP server subprocess and its JSON-RPC plumbing.
// A dedicated reader goroutine owns stdout (io.Reader is not concurrency-safe);
// callers register a pending[id] slot before writing, and the reader routes
// responses back by id — the classic reactor pattern also used by internal/mcp.
type serverConn struct {
	language string
	rootURI  string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	timeout  time.Duration
	logger   *zap.Logger

	nextID  atomic.Int64
	writeMu sync.Mutex // serializes framed writes to stdin

	mu      sync.Mutex
	pending map[int64]chan *jsonrpcResponse
	closed  bool

	// opened tracks documents already sent via textDocument/didOpen so we can
	// avoid re-opening and satisfy servers that require an open document.
	openedMu sync.Mutex
	opened   map[string]bool
}

// writeFrame serializes a JSON-RPC message using the LSP base protocol:
//
//	Content-Length: N\r\n\r\n<json>
//
// A single writeMu guarantees at most one writer touches stdin at a time.
func (sc *serverConn) writeFrame(v interface{}) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	sc.writeMu.Lock()
	defer sc.writeMu.Unlock()
	if sc.closed {
		return fmt.Errorf("lsp server %q connection closed", sc.language)
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))
	if _, err := io.WriteString(sc.stdin, header); err != nil {
		return err
	}
	_, err = sc.stdin.Write(payload)
	return err
}

// readLoop is the single owner of stdout. It decodes Content-Length framed
// messages and routes id-bearing responses to their pending slot. Server
// -initiated requests/notifications (Method != "") are acknowledged/ignored
// so gopls and friends do not block waiting for a reply.
func (sc *serverConn) readLoop(stdout io.Reader) {
	reader := bufio.NewReader(stdout)
	for {
		length, err := readContentLength(reader)
		if err != nil {
			sc.failAll(err)
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			sc.failAll(err)
			return
		}
		var resp jsonrpcResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			sc.logger.Warn("lsp: skip malformed frame", zap.Error(err))
			continue
		}
		// Server-initiated request that expects a reply (has id + method):
		// reply with null result so the server does not stall.
		if resp.Method != "" {
			if resp.ID != nil {
				_ = sc.writeFrame(jsonrpcResponse{JSONRPC: "2.0", ID: resp.ID, Result: json.RawMessage("null")})
			}
			continue
		}
		if resp.ID == nil {
			continue // notification without id — nothing to route
		}
		sc.mu.Lock()
		ch, ok := sc.pending[*resp.ID]
		if ok {
			delete(sc.pending, *resp.ID)
		}
		sc.mu.Unlock()
		if ok {
			r := resp
			ch <- &r
		}
	}
}

// readContentLength parses LSP headers and returns the body byte length.
func readContentLength(r *bufio.Reader) (int, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" { // blank line terminates headers
			if length < 0 {
				return 0, fmt.Errorf("lsp: missing Content-Length header")
			}
			return length, nil
		}
		if strings.HasPrefix(line, "Content-Length:") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			n, err := strconv.Atoi(v)
			if err != nil {
				return 0, fmt.Errorf("lsp: bad Content-Length %q: %w", v, err)
			}
			length = n
		}
	}
}

// failAll wakes every pending caller with an error (server died / EOF).
func (sc *serverConn) failAll(cause error) {
	sc.mu.Lock()
	if !sc.closed {
		sc.closed = true
	}
	pending := sc.pending
	sc.pending = make(map[int64]chan *jsonrpcResponse)
	sc.mu.Unlock()
	for id, ch := range pending {
		ch <- &jsonrpcResponse{ID: &id, Error: &jsonrpcError{Code: -32000, Message: "lsp connection lost: " + cause.Error()}}
	}
}

// call sends a request and blocks until the matching response, ctx timeout,
// or connection loss. The pending slot is always cleaned up to avoid leaks.
func (sc *serverConn) call(ctx context.Context, method string, params interface{}, out interface{}) error {
	id := sc.nextID.Add(1)
	respCh := make(chan *jsonrpcResponse, 1)

	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return fmt.Errorf("lsp server %q not running", sc.language)
	}
	sc.pending[id] = respCh
	sc.mu.Unlock()

	req := jsonrpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
	if err := sc.writeFrame(req); err != nil {
		sc.mu.Lock()
		delete(sc.pending, id)
		sc.mu.Unlock()
		return err
	}

	if sc.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sc.timeout)
		defer cancel()
	}

	select {
	case <-ctx.Done():
		sc.mu.Lock()
		delete(sc.pending, id)
		sc.mu.Unlock()
		return ctx.Err()
	case resp := <-respCh:
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}

// notify sends a fire-and-forget JSON-RPC notification (no id, no reply).
func (sc *serverConn) notify(method string, params interface{}) error {
	return sc.writeFrame(jsonrpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// ---- Client interface: lifecycle ----

func (c *client) Initialize(ctx context.Context, language, rootPath string) error {
	c.mu.Lock()
	if _, exists := c.servers[language]; exists {
		c.mu.Unlock()
		return nil // already initialized (idempotent)
	}
	scfg, ok := c.cfg.Servers[language]
	if !ok || scfg.Command == "" {
		c.mu.Unlock()
		return fmt.Errorf("no LSP server configured for language %q", language)
	}
	c.mu.Unlock()

	cmd := exec.Command(scfg.Command, scfg.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("lsp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("lsp: stdout pipe: %w", err)
	}
	cmd.Stderr = nil // discard server diagnostics; keep our logs clean
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lsp: start %q: %w", scfg.Command, err)
	}

	timeout := time.Duration(c.cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	sc := &serverConn{
		language: language,
		rootURI:  fileToURI(rootPath),
		cmd:      cmd,
		stdin:    stdin,
		timeout:  timeout,
		logger:   c.logger.With(zap.String("lsp_lang", language)),
		pending:  make(map[int64]chan *jsonrpcResponse),
		opened:   make(map[string]bool),
	}
	go sc.readLoop(stdout)

	// LSP handshake: initialize → initialized.
	initParams := map[string]interface{}{
		"processId": nil,
		"rootUri":   sc.rootURI,
		"capabilities": map[string]interface{}{
			"textDocument": map[string]interface{}{
				"definition":     map[string]interface{}{},
				"references":     map[string]interface{}{},
				"hover":          map[string]interface{}{},
				"rename":         map[string]interface{}{},
				"documentSymbol": map[string]interface{}{"hierarchicalDocumentSymbolSupport": true},
			},
		},
	}
	if err := sc.call(ctx, "initialize", initParams, nil); err != nil {
		_ = sc.close()
		return fmt.Errorf("lsp: initialize handshake failed: %w", err)
	}
	if err := sc.notify("initialized", map[string]interface{}{}); err != nil {
		_ = sc.close()
		return fmt.Errorf("lsp: initialized notify failed: %w", err)
	}

	c.mu.Lock()
	c.servers[language] = sc
	c.mu.Unlock()
	c.logger.Info("LSP server initialized", zap.String("language", language), zap.String("command", scfg.Command))
	return nil
}

func (c *client) Shutdown(language string) error {
	c.mu.Lock()
	sc, ok := c.servers[language]
	if ok {
		delete(c.servers, language)
	}
	c.mu.Unlock()
	if !ok {
		return nil
	}
	return sc.close()
}

func (c *client) ShutdownAll() error {
	c.mu.Lock()
	servers := c.servers
	c.servers = make(map[string]*serverConn)
	c.mu.Unlock()
	for _, sc := range servers {
		_ = sc.close()
	}
	return nil
}

// close performs a best-effort graceful LSP shutdown then kills the process.
func (sc *serverConn) close() error {
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return nil
	}
	sc.closed = true
	sc.mu.Unlock()

	// Best-effort graceful shutdown (bounded, ignore errors).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = sc.call(ctx, "shutdown", nil, nil)
	_ = sc.notify("exit", nil)

	_ = sc.stdin.Close()
	if sc.cmd != nil && sc.cmd.Process != nil {
		_ = sc.cmd.Process.Kill()
		_ = sc.cmd.Wait()
	}
	return nil
}

// ---- LSP wire types (subset) ----

type wirePosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type wireRange struct {
	Start wirePosition `json:"start"`
	End   wirePosition `json:"end"`
}

type wireLocation struct {
	URI   string    `json:"uri"`
	Range wireRange `json:"range"`
}

func (l wireLocation) toLocation() Location {
	return Location{
		URI:       l.URI,
		StartLine: l.Range.Start.Line,
		StartCol:  l.Range.Start.Character,
		EndLine:   l.Range.End.Line,
		EndCol:    l.Range.End.Character,
	}
}

func (r wireRange) toRange() Range {
	return Range{
		Start: Position{Line: r.Start.Line, Character: r.Start.Character},
		End:   Position{Line: r.End.Line, Character: r.End.Character},
	}
}

// textDocumentPositionParams is the common params shape for definition/
// references/hover/rename requests.
func posParams(uri string, line, col int) map[string]interface{} {
	return map[string]interface{}{
		"textDocument": map[string]string{"uri": uri},
		"position":     map[string]int{"line": line, "character": col},
	}
}

// resolveConn returns the running server connection that owns the given URI's
// language. It maps URIs by configured Languages (file extension), falling back
// to the sole running server when only one is active.
func (c *client) resolveConn(uri string) (*serverConn, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.servers) == 0 {
		return nil, fmt.Errorf("no LSP server running")
	}
	lang := c.languageForURI(uri)
	if lang != "" {
		if sc, ok := c.servers[lang]; ok {
			return sc, nil
		}
	}
	if len(c.servers) == 1 {
		for _, sc := range c.servers {
			return sc, nil
		}
	}
	return nil, fmt.Errorf("no LSP server running for %q", uri)
}

// languageForURI matches a file URI against configured server language keys by
// their declared extensions (Languages field holds extensions like ".go").
func (c *client) languageForURI(uri string) string {
	path := uriToFile(uri)
	for lang, scfg := range c.cfg.Servers {
		for _, ext := range scfg.Languages {
			if strings.HasSuffix(path, ext) {
				return lang
			}
		}
	}
	return ""
}

func fileToURI(path string) string {
	if strings.HasPrefix(path, "file://") {
		return path
	}
	return "file://" + path
}

func uriToFile(uri string) string {
	return strings.TrimPrefix(uri, "file://")
}

// ---- Client interface: semantic operations ----

// GotoDefinition resolves textDocument/definition. The result may be a single
// Location or an array of Locations depending on the server, so we decode into
// json.RawMessage first and try both shapes.
func (c *client) GotoDefinition(ctx context.Context, uri string, line, col int) ([]Location, error) {
	sc, err := c.resolveConn(uri)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := sc.call(ctx, "textDocument/definition", posParams(uri, line, col), &raw); err != nil {
		return nil, err
	}
	return decodeLocations(raw), nil
}

// FindReferences resolves textDocument/references (always an array).
func (c *client) FindReferences(ctx context.Context, uri string, line, col int) ([]Location, error) {
	sc, err := c.resolveConn(uri)
	if err != nil {
		return nil, err
	}
	params := posParams(uri, line, col)
	params["context"] = map[string]interface{}{"includeDeclaration": true}
	var wires []wireLocation
	if err := sc.call(ctx, "textDocument/references", params, &wires); err != nil {
		return nil, err
	}
	out := make([]Location, 0, len(wires))
	for _, w := range wires {
		out = append(out, w.toLocation())
	}
	return out, nil
}

// Rename resolves textDocument/rename into a WorkspaceEdit.
func (c *client) Rename(ctx context.Context, uri string, line, col int, newName string) (*WorkspaceEdit, error) {
	sc, err := c.resolveConn(uri)
	if err != nil {
		return nil, err
	}
	params := posParams(uri, line, col)
	params["newName"] = newName
	var wire struct {
		Changes map[string][]struct {
			Range   wireRange `json:"range"`
			NewText string    `json:"newText"`
		} `json:"changes"`
	}
	if err := sc.call(ctx, "textDocument/rename", params, &wire); err != nil {
		return nil, err
	}
	edit := &WorkspaceEdit{Changes: make(map[string][]TextEdit, len(wire.Changes))}
	for u, edits := range wire.Changes {
		converted := make([]TextEdit, 0, len(edits))
		for _, e := range edits {
			converted = append(converted, TextEdit{Range: e.Range.toRange(), NewText: e.NewText})
		}
		edit.Changes[u] = converted
	}
	return edit, nil
}

// Hover resolves textDocument/hover. The contents field is a MarkupContent
// object (LSP 3.x); we extract its plain value.
func (c *client) Hover(ctx context.Context, uri string, line, col int) (*HoverResult, error) {
	sc, err := c.resolveConn(uri)
	if err != nil {
		return nil, err
	}
	var wire struct {
		Contents struct {
			Value string `json:"value"`
		} `json:"contents"`
		Range *wireRange `json:"range"`
	}
	if err := sc.call(ctx, "textDocument/hover", posParams(uri, line, col), &wire); err != nil {
		return nil, err
	}
	res := &HoverResult{Contents: wire.Contents.Value}
	if wire.Range != nil {
		r := wire.Range.toRange()
		res.Range = &r
	}
	return res, nil
}

// DocumentSymbols resolves textDocument/documentSymbol. Servers may return the
// hierarchical DocumentSymbol[] shape or the flat SymbolInformation[] shape;
// we handle both.
func (c *client) DocumentSymbols(ctx context.Context, uri string) ([]SymbolInfo, error) {
	sc, err := c.resolveConn(uri)
	if err != nil {
		return nil, err
	}
	params := map[string]interface{}{"textDocument": map[string]string{"uri": uri}}
	var raw json.RawMessage
	if err := sc.call(ctx, "textDocument/documentSymbol", params, &raw); err != nil {
		return nil, err
	}
	return decodeSymbols(raw), nil
}

// DidChange notifies the server of a full-document change (whole-document sync).
func (c *client) DidChange(ctx context.Context, uri, content string) error {
	sc, err := c.resolveConn(uri)
	if err != nil {
		return err
	}
	params := map[string]interface{}{
		"textDocument":   map[string]interface{}{"uri": uri, "version": time.Now().UnixNano()},
		"contentChanges": []map[string]interface{}{{"text": content}},
	}
	return sc.notify("textDocument/didChange", params)
}

// ---- decode helpers ----

// decodeLocations accepts either a single Location object or an array.
func decodeLocations(raw json.RawMessage) []Location {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var arr []wireLocation
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]Location, 0, len(arr))
		for _, w := range arr {
			out = append(out, w.toLocation())
		}
		return out
	}
	var single wireLocation
	if err := json.Unmarshal(raw, &single); err == nil {
		return []Location{single.toLocation()}
	}
	return nil
}

// wireDocumentSymbol is the hierarchical DocumentSymbol shape.
type wireDocumentSymbol struct {
	Name       string               `json:"name"`
	Detail     string               `json:"detail"`
	Kind       int                  `json:"kind"`
	Deprecated bool                 `json:"deprecated"`
	Range      wireRange            `json:"range"`
	Children   []wireDocumentSymbol `json:"children"`
}

func (w wireDocumentSymbol) toSymbolInfo() SymbolInfo {
	si := SymbolInfo{
		Name:       w.Name,
		Kind:       w.Kind,
		Range:      w.Range.toRange(),
		Detail:     w.Detail,
		Deprecated: w.Deprecated,
	}
	for _, ch := range w.Children {
		si.Children = append(si.Children, ch.toSymbolInfo())
	}
	return si
}

// wireSymbolInformation is the flat SymbolInformation shape.
type wireSymbolInformation struct {
	Name     string       `json:"name"`
	Kind     int          `json:"kind"`
	Location wireLocation `json:"location"`
}

// decodeSymbols accepts DocumentSymbol[] (hierarchical) or
// SymbolInformation[] (flat), preferring the hierarchical shape.
func decodeSymbols(raw json.RawMessage) []SymbolInfo {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var hier []wireDocumentSymbol
	if err := json.Unmarshal(raw, &hier); err == nil && len(hier) > 0 && hier[0].Name != "" {
		out := make([]SymbolInfo, 0, len(hier))
		for _, w := range hier {
			out = append(out, w.toSymbolInfo())
		}
		return out
	}
	var flat []wireSymbolInformation
	if err := json.Unmarshal(raw, &flat); err == nil {
		out := make([]SymbolInfo, 0, len(flat))
		for _, w := range flat {
			out = append(out, SymbolInfo{Name: w.Name, Kind: w.Kind, Range: w.Location.Range.toRange()})
		}
		return out
	}
	return nil
}