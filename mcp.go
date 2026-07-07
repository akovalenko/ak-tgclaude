package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// mcpServerName is the MCP server the dispatcher exposes to the responder; the
// tools are addressed as mcp__<mcpServerName>__<tool>.
const mcpServerName = "tg"

// The tool names on that server. The responder needs their fully-qualified forms
// (mcp__tg__<tool>) in BOTH its agent `tools:` frontmatter (availability gate)
// and --allowedTools (permission gate under --permission-mode dontAsk).
const (
	toolSendMessage  = "send_message"
	toolSendCode     = "send_code"
	toolSendDocument = "send_document"
)

// mcpProtocolVersion is advertised when the client does not request one. Claude
// Code negotiates by echoing; we prefer the client's requested version.
const mcpProtocolVersion = "2025-06-18"

// qualifiedTool returns the fully-qualified MCP tool name (mcp__tg__<tool>).
func qualifiedTool(tool string) string { return "mcp__" + mcpServerName + "__" + tool }

// mcpTools are the fully-qualified send tools the responder is granted.
var mcpTools = []string{
	qualifiedTool(toolSendMessage),
	qualifiedTool(toolSendCode),
	qualifiedTool(toolSendDocument),
}

// mcpRoute binds one invocation's capability token to its Telegram route and the
// directory its document attachments must live in (path confinement). delivered
// counts the messages this invocation actually sent, so the dispatcher can tell a
// responder that emitted nothing (dumped its answer into its discarded final text)
// from one that delivered — the delivery guard (require_delivery) keys on it.
type mcpRoute struct {
	route     Route
	docDir    string
	delivered int
}

// mcpServer is the dispatcher-owned MCP-over-HTTP transport: a long-lived local
// JSON-RPC server the per-invocation responders call as clients. It replaces the
// outbox spool — the responder emits by calling the send_* tools and the server
// delivers to Telegram synchronously, returning the message_id (or a tool error
// the model can act on).
//
// The Telegram ROUTE is never chosen by the responder. The dispatcher mints a
// bearer token per invocation and maps it to that invocation's route; the tool
// call carries no chat_id, and the server resolves the route from the token in
// the request's Authorization header. That token IS the route capability (as the
// per-invocation outbox dir was in the spool design). Claude Code attaches the
// header under the hood, so the token never enters the model's context.
type mcpServer struct {
	sender   Sender
	uploader *uploader // large-file fallback for send_document; nil = off
	version  string
	debug    bool // log the handshake chatter (every request, initialize, tools/list)
	ln       net.Listener
	srv      *http.Server

	transcripts *TranscriptStore // bot-side transcript append (nil => feature off); set after construction

	overflow string // oversized-text policy "spill"|"error" (Config.Overflow); set after construction, "" => spill

	mu     sync.Mutex
	routes map[string]mcpRoute
}

// newMCPServer binds a localhost listener and starts serving JSON-RPC at /mcp. up
// is the optional large-file uploader (nil = the send_document fallback is off).
func newMCPServer(sender Sender, version string, debug bool, up *uploader) (*mcpServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mcp: listen: %w", err)
	}
	m := &mcpServer{sender: sender, uploader: up, version: version, debug: debug, ln: ln, routes: make(map[string]mcpRoute)}
	// Catch-all (not just "/mcp") so EVERY request is logged in handle, including
	// one to a wrong path — which the mux would otherwise answer 404 without
	// logging, hiding a client that dials the wrong URL.
	mux := http.NewServeMux()
	mux.HandleFunc("/", m.handle)
	m.srv = &http.Server{Handler: mux}
	go func() {
		if err := m.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("ak-tgclaude: mcp server: %v", err)
		}
	}()
	return m, nil
}

// URL is the endpoint the responder's --mcp-config points at.
func (m *mcpServer) URL() string { return "http://" + m.ln.Addr().String() + "/mcp" }

// Register mints a capability token for one invocation, mapping it to the
// invocation's route and document directory. Unregister drops it on responder
// exit so a leaked token cannot be reused.
func (m *mcpServer) Register(r Route, docDir string) (string, error) {
	tok, err := randomToken()
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.routes[tok] = mcpRoute{route: r, docDir: docDir}
	m.mu.Unlock()
	return tok, nil
}

func (m *mcpServer) Unregister(token string) {
	m.mu.Lock()
	delete(m.routes, token)
	m.mu.Unlock()
}

func (m *mcpServer) lookup(token string) (mcpRoute, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.routes[token]
	return r, ok
}

// recordDelivered increments the invocation's delivered-message count on a
// successful send. Called from callTool under a still-registered token.
func (m *mcpServer) recordDelivered(token string) {
	m.mu.Lock()
	if r, ok := m.routes[token]; ok {
		r.delivered++
		m.routes[token] = r
	}
	m.mu.Unlock()
}

// DeliveredCount reports how many messages the invocation identified by token has
// sent so far (0 if the token is unknown). The dispatcher reads it after the
// responder returns to run the delivery guard.
func (m *mcpServer) DeliveredCount(token string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routes[token].delivered
}

// Close stops the HTTP server.
func (m *mcpServer) Close() error { return m.srv.Close() }

// debugf logs the MCP handshake chatter — only under --debug. The operationally
// meaningful lines (tools/call outcomes, unauthorized, unknown method) log
// unconditionally.
func (m *mcpServer) debugf(format string, args ...any) {
	if m.debug {
		log.Printf(format, args...)
	}
}

// randomToken returns a 256-bit random bearer token, hex-encoded.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mcp: minting token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// bearer extracts the token from an "Authorization: Bearer <token>" header.
func bearer(h string) string {
	const p = "Bearer "
	if len(h) >= len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// rpcRequest is one JSON-RPC 2.0 message. A notification omits id.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// rpcResponse is a JSON-RPC 2.0 response envelope.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// handle serves one JSON-RPC message over HTTP POST. A request (has id) gets a
// JSON response; a notification (no id) gets 202 with no body — the streamable-
// http shape Claude Code's client expects, without SSE or session headers.
func (m *mcpServer) handle(w http.ResponseWriter, req *http.Request) {
	// Log EVERY incoming request up front — method + path + whether it carried an
	// Authorization header — so any connection attempt is visible, including a GET
	// probe (which we answer 405) and a request with a bad/absent token. This
	// disambiguates "no mcp: lines" between "the client never dialed" and "it
	// dialed but we answered before logging".
	m.debugf("ak-tgclaude: mcp: <- %s %s auth=%t", req.Method, req.URL.Path, req.Header.Get("Authorization") != "")
	if req.URL.Path != "/mcp" {
		http.NotFound(w, req)
		return
	}
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		writeRPCError(w, nil, -32700, "read error")
		return
	}
	var rpc rpcRequest
	if err := json.Unmarshal(body, &rpc); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}

	// Every request carries this invocation's bearer token (the dispatcher wrote
	// it into the responder's --mcp-config). Resolve the route now; tools/call
	// needs it, the handshake methods do not, but an unknown token is rejected
	// uniformly so a stray client cannot even enumerate the tools.
	tok := bearer(req.Header.Get("Authorization"))
	rt, authed := m.lookup(tok)

	// Notifications (no id) are acknowledged with 202 and no body.
	if len(rpc.ID) == 0 {
		m.debugf("ak-tgclaude: mcp: notification %q", rpc.Method)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if !authed {
		log.Printf("ak-tgclaude: mcp: %s: unauthorized (token=%s)", rpc.Method, tokenPrefix(tok))
		writeRPCError(w, rpc.ID, -32001, "unauthorized")
		return
	}

	switch rpc.Method {
	case "initialize":
		m.debugf("ak-tgclaude: mcp: initialize chat=%d", rt.route.ChatID)
		writeRPCResult(w, rpc.ID, m.initializeResult(rpc.Params))
	case "tools/list":
		m.debugf("ak-tgclaude: mcp: tools/list chat=%d", rt.route.ChatID)
		writeRPCResult(w, rpc.ID, toolsListResult())
	case "tools/call":
		writeRPCResult(w, rpc.ID, m.callTool(req.Context(), tok, rt, rpc.Params))
	case "ping":
		writeRPCResult(w, rpc.ID, map[string]any{})
	default:
		log.Printf("ak-tgclaude: mcp: unknown method %q", rpc.Method)
		writeRPCError(w, rpc.ID, -32601, "method not found: "+rpc.Method)
	}
}

// tokenPrefix returns a short, log-safe prefix of a bearer token (it is a route
// capability, not the bot secret, but there is no reason to log it in full).
func tokenPrefix(t string) string {
	switch {
	case t == "":
		return "(none)"
	case len(t) > 8:
		return t[:8] + "…"
	default:
		return t
	}
}

// writeRPCResult and writeRPCError encode a JSON-RPC response envelope.
func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: normalizeID(id), Result: result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: normalizeID(id), Error: &rpcError{Code: code, Message: msg}})
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(&resp)
}

// normalizeID maps an absent id to JSON null so the envelope stays valid.
func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// initializeResult echoes the client's requested protocol version (falling back
// to ours) and advertises the tools capability.
func (m *mcpServer) initializeResult(params json.RawMessage) map[string]any {
	version := mcpProtocolVersion
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
			version = p.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "ak-tgclaude", "version": m.version},
	}
}

// toolsListResult describes the send tools. The route (chat/reply) is never a
// parameter — the dispatcher pins it per invocation.
func toolsListResult() map[string]any {
	strProp := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	boolProp := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	// progressProp is shared by all send tools: an "along the way" note that is
	// delivered but does not count as the answer for the delivery guard.
	progressProp := boolProp("Mark this as an along-the-way progress note (not your answer): it is delivered, but does NOT count as your reply for the delivery check. Default false.")
	return map[string]any{"tools": []map[string]any{
		{
			"name":        toolSendMessage,
			"description": "Send a text message as the bot's reply. The chat and reply target are pinned by the dispatcher — you never choose them. Supply the body either inline (text) or as a file in your outbox (text_file) — exactly one.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text":      strProp("The message body, inline. Omit when you pass text_file."),
					"text_file": strProp("Instead of text: the basename of a file in your outbox holding the body. Use this for content produced by another tool (e.g. gitlab-links output with commit SHAs) so it reaches Telegram verbatim — never retyped through your reply, where a stray edit could corrupt it. Exactly one of text / text_file."),
					"html":      boolProp("Render as Telegram HTML (parse_mode=HTML); supply valid, escaped HTML. Default false (plain text)."),
					"silent":    boolProp("Deliver without a notification. Default false."),
					"progress":  progressProp,
				},
			},
		},
		{
			"name":        toolSendCode,
			"description": "Send a preformatted code block. It is wrapped in <pre><code> and escaped for you, and spills to a document if it exceeds Telegram's size limit. Supply the body either inline (code) or as a file in your outbox (code_file) — exactly one.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code":      strProp("The raw code/preformatted text, inline (do not pre-wrap it in HTML). Omit when you pass code_file."),
					"code_file": strProp("Instead of code: the basename of a file in your outbox holding the raw code. Use this to echo a file verbatim without retyping it. Exactly one of code / code_file."),
					"language":  strProp("Optional source language tag (e.g. go, python)."),
					"caption":   strProp("Optional line shown before the block."),
					"silent":    boolProp("Deliver without a notification. Default false."),
					"progress":  progressProp,
				},
			},
		},
		{
			"name":        toolSendDocument,
			"description": "Send a file attachment. Write the file into your outbox directory first (with the Write tool), then pass its path here.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":     strProp("Path to the file in your outbox directory (only files there can be sent)."),
					"filename": strProp("Optional display name (default: the file's base name)."),
					"caption":  strProp("Optional caption."),
					"silent":   boolProp("Deliver without a notification. Default false."),
					"progress": progressProp,
				},
				"required": []string{"path"},
			},
		},
	}}
}

// callTool dispatches a tools/call: it builds a descriptor from the arguments and
// delivers it on the invocation's route, returning the Telegram message_id. A bad
// call (unknown tool, missing field, out-of-scope path) or a Telegram rejection
// is returned as a tool error (isError) the model can see and often fix — not a
// JSON-RPC protocol error.
func (m *mcpServer) callTool(ctx context.Context, tok string, rt mcpRoute, params json.RawMessage) map[string]any {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		log.Printf("ak-tgclaude: mcp: tools/call chat=%d: bad params: %v", rt.route.ChatID, err)
		return toolError("invalid tools/call params: " + err.Error())
	}
	d, err := descriptorFromCall(call.Name, call.Arguments, rt.docDir)
	if err != nil {
		log.Printf("ak-tgclaude: mcp: tools/call %s chat=%d: rejected: %v", call.Name, rt.route.ChatID, err)
		return toolError(err.Error())
	}
	ids, err := sendDescriptor(ctx, d, rt.route, m.sender, m.uploader, m.overflow)
	if err != nil {
		// An upload-path failure (too large, uploader crashed, no URL) is surfaced
		// to the model verbatim; only a genuine Telegram API error gets the "Telegram
		// rejected" framing.
		var ue *uploadError
		if errors.As(err, &ue) {
			log.Printf("ak-tgclaude: mcp: tools/call %s chat=%d: upload error: %v", call.Name, rt.route.ChatID, err)
			return toolError(ue.Error())
		}
		// Our pre-send HTML guard: surface it verbatim (all bad tags) so the model can
		// fix them, rather than under the "Telegram rejected" framing.
		var he *htmlError
		if errors.As(err, &he) {
			log.Printf("ak-tgclaude: mcp: tools/call %s chat=%d: html guard: %v", call.Name, rt.route.ChatID, err)
			return toolError(he.Error())
		}
		// Overflow policy "error": the reply is too long and will not split. Surface it
		// verbatim so the model shortens it, like the HTML guard above.
		var oe *oversizeError
		if errors.As(err, &oe) {
			log.Printf("ak-tgclaude: mcp: tools/call %s chat=%d: oversize: %v", call.Name, rt.route.ChatID, err)
			return toolError(oe.Error())
		}
		log.Printf("ak-tgclaude: mcp: tools/call %s chat=%d: telegram error: %v", call.Name, rt.route.ChatID, err)
		return toolError("Telegram rejected the message: " + deliveryError(err))
	}
	// A progress note is delivered but does NOT count as the answer: the delivery
	// guard keys on real sends, so a pre-reporting responder can narrate without
	// blinding the "answer never got sent" check.
	if !d.Progress {
		m.recordDelivered(tok)
	}
	// Record the bot's turn: every delivered message (progress notes too — they carry
	// real ids a later reply_to may point at). A message that split across several
	// sends writes one record per piece — the anchor (first id) carries the full text
	// and threads to the incoming message; each later piece is a light stub pointing
	// at the anchor via PartOf, so the answer is stored once yet every physical
	// message_id resolves. Non-fatal on error.
	anchor := ids[0]
	if m.transcripts != nil {
		for i, mid := range ids {
			brec := TranscriptRecord{MsgID: mid, TS: time.Now(), Role: "bot"}
			if i == 0 {
				brec.ReplyTo = rt.route.ReplyTo
				brec.Text = botText(d)
				brec.Attach = botAttach(d)
			} else {
				brec.PartOf = anchor
			}
			if err := m.transcripts.Append(rt.route.ChatID, brec, nil); err != nil {
				log.Printf("ak-tgclaude: transcript(bot) chat=%d msg=%d: %v", rt.route.ChatID, mid, err)
			}
		}
	}
	log.Printf("ak-tgclaude: mcp: tools/call %s chat=%d -> message_id=%d parts=%d progress=%t", call.Name, rt.route.ChatID, anchor, len(ids), d.Progress)
	return toolText(fmt.Sprintf("delivered (message_id %d)", anchor))
}

// botText is the transcript text for a delivered descriptor: the message body, the
// code, or a document's caption (the file itself is recorded as an attachment).
func botText(d *Descriptor) string {
	switch d.Kind {
	case KindText:
		return d.Text
	case KindCode:
		return d.Code
	case KindDocument:
		return d.Caption
	}
	return ""
}

// botAttach records a delivered document as transcript metadata (no bytes); other
// kinds carry none.
func botAttach(d *Descriptor) []TranscriptAttach {
	if d.Kind != KindDocument {
		return nil
	}
	name := d.Filename
	if name == "" {
		name = filepath.Base(d.Path)
	}
	var size int64
	if fi, err := os.Lstat(d.Path); err == nil {
		size = fi.Size()
	}
	return []TranscriptAttach{{Kind: "document", Name: name, Size: size}}
}

// descriptorFromCall builds a validated Descriptor from a tool name and its JSON
// arguments. A document path is confined to docDir (basename only) so the server,
// which runs unsandboxed, cannot be steered into attaching an arbitrary host file.
func descriptorFromCall(name string, args json.RawMessage, docDir string) (*Descriptor, error) {
	switch name {
	case toolSendMessage:
		var a struct {
			Text     string `json:"text"`
			TextFile string `json:"text_file"`
			HTML     bool   `json:"html"`
			Silent   bool   `json:"silent"`
			Progress bool   `json:"progress"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("invalid %s arguments: %w", name, err)
		}
		text, err := bodyFromArgOrFile(name, "text", a.Text, a.TextFile, docDir)
		if err != nil {
			return nil, err
		}
		d := &Descriptor{Kind: KindText, Text: text, Silent: a.Silent, Progress: a.Progress}
		if a.HTML {
			d.Format = FormatHTML
		}
		return d, d.validate()
	case toolSendCode:
		var a struct {
			Code     string `json:"code"`
			CodeFile string `json:"code_file"`
			Language string `json:"language"`
			Caption  string `json:"caption"`
			Silent   bool   `json:"silent"`
			Progress bool   `json:"progress"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("invalid %s arguments: %w", name, err)
		}
		code, err := bodyFromArgOrFile(name, "code", a.Code, a.CodeFile, docDir)
		if err != nil {
			return nil, err
		}
		d := &Descriptor{Kind: KindCode, Code: code, Language: a.Language, Caption: a.Caption, Silent: a.Silent, Progress: a.Progress}
		return d, d.validate()
	case toolSendDocument:
		var a struct {
			Path     string `json:"path"`
			Filename string `json:"filename"`
			Caption  string `json:"caption"`
			Silent   bool   `json:"silent"`
			Progress bool   `json:"progress"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("invalid %s arguments: %w", name, err)
		}
		full, err := confineDoc(docDir, a.Path)
		if err != nil {
			return nil, err
		}
		d := &Descriptor{Kind: KindDocument, Path: full, Filename: a.Filename, Caption: a.Caption, Silent: a.Silent, Progress: a.Progress}
		return d, d.validate()
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

// confineDoc resolves a document path to a real file inside docDir. It takes only
// the basename and joins it to docDir, so any directory component (including an
// absolute path like /home/.../.ssh/id_rsa) collapses to a name that must exist
// in the outbox — the periphery the sandbox gave the spool `send` for free.
func confineDoc(docDir, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("document path is empty")
	}
	base := filepath.Base(p)
	if base == "." || base == string(filepath.Separator) {
		return "", fmt.Errorf("invalid document path %q", p)
	}
	full := filepath.Join(docDir, base)
	info, err := os.Lstat(full)
	if err != nil {
		return "", fmt.Errorf("attachment %q not found in your outbox directory (write it there first)", base)
	}
	if info.IsDir() {
		return "", fmt.Errorf("attachment %q is a directory", base)
	}
	// A symlink is never a legitimate outbox attachment, and the dispatcher that
	// opens it runs UNSANDBOXED — a link to a host secret (bot.toml, ~/.ssh/id_rsa)
	// would be read in the clear, past the responder's inode-pinned sandbox masks
	// (a symlink's target is not read at creation, so those masks never fire on it).
	// Refuse it here for a clear early error; the O_NOFOLLOW open at send time (see
	// openNoFollow) is the race-free enforcement that a symlink swap cannot slip.
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("attachment %q is a symlink; outbox files must be regular files", base)
	}
	return full, nil
}

// openNoFollow opens full for reading, refusing to traverse a final-component
// symlink (O_NOFOLLOW). It is the single vetted open for an outbox file: the caller
// hands the returned *os.File downstream (the multipart body, the uploader's
// inherited fd) instead of re-opening the path, so there is no check-then-open
// window a symlink swap could exploit — the fd is pinned to the inode we vetted.
func openNoFollow(full string) (*os.File, error) {
	f, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if fi.IsDir() {
		f.Close()
		return nil, fmt.Errorf("%q is a directory", filepath.Base(full))
	}
	return f, nil
}

// maxBodyFileBytes caps a text_file / code_file body so a stray huge file cannot
// exhaust memory. A legitimate over-limit answer is far smaller — it spills only
// because it exceeds Telegram's 4096-char message cap, not because it is megabytes.
const maxBodyFileBytes = 4 << 20 // 4 MiB

// readOutboxBody reads a message body from a file in the invocation's outbox, path-
// confined exactly like a document attachment (basename only, must already exist). It
// lets a caller hand the body as a PATH instead of an inline string, so content
// produced by another tool (e.g. gitlab-links output carrying commit SHAs) reaches
// Telegram verbatim — never laundered through the model's tokens, where a stray space
// in a SHA would break a permalink.
func readOutboxBody(docDir, name string) (string, error) {
	full, err := confineDoc(docDir, name)
	if err != nil {
		return "", err
	}
	f, err := openNoFollow(full)
	if err != nil {
		return "", fmt.Errorf("reading body file %q: %w", filepath.Base(name), err)
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > maxBodyFileBytes {
		return "", fmt.Errorf("body file %q too large (%d bytes, max %d)", filepath.Base(name), info.Size(), maxBodyFileBytes)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("reading body file %q: %w", filepath.Base(name), err)
	}
	return string(b), nil
}

// bodyFromArgOrFile resolves a message body supplied EITHER inline (inlineVal) OR as
// the basename of an outbox file (fileVal); exactly one may be set. field names the
// inline argument for error text ("text" / "code"); the file argument is field+"_file".
// An empty result is passed through so the descriptor's validate() owns the single
// "empty body" error.
func bodyFromArgOrFile(tool, field, inlineVal, fileVal, docDir string) (string, error) {
	switch {
	case inlineVal != "" && fileVal != "":
		return "", fmt.Errorf("%s: provide either %s or %s_file, not both", tool, field, field)
	case fileVal != "":
		return readOutboxBody(docDir, fileVal)
	default:
		return inlineVal, nil
	}
}

// toolText and toolError build the CallToolResult content for a success / failure.
func toolText(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func toolError(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": true}
}

// buildMCPConfig returns the inline --mcp-config JSON wiring the responder to the
// dispatcher's MCP server with this invocation's capability token in the
// Authorization header. Claude Code attaches the header to every MCP request.
func buildMCPConfig(url, token string) string {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			mcpServerName: map[string]any{
				"type":    "http",
				"url":     url,
				"headers": map[string]any{"Authorization": "Bearer " + token},
			},
		},
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

// mcpLoopbackClient talks to the dispatcher's own MCP server on localhost. It
// forces Proxy:nil so a host HTTP(S)_PROXY does not swallow the loopback request
// (the same trap the responder avoids via NO_PROXY) — the stub runs inside the
// dispatcher, which may have a proxy configured for the Telegram/API traffic.
var mcpLoopbackClient = &http.Client{Transport: &http.Transport{Proxy: nil}}

// mcpStubSend delivers text by making a real send_message tools/call to the
// dispatcher's MCP server — the stub responder's path, so --responder stub
// exercises the actual transport (HTTP + auth + route resolution + delivery)
// without spawning claude. It returns an error on transport failure, a JSON-RPC
// error, or a tool error (isError).
func mcpStubSend(ctx context.Context, url, token, text string) error {
	if url == "" || token == "" {
		return fmt.Errorf("stub: no MCP endpoint configured")
	}
	reqBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolSendMessage,
			"arguments": map[string]any{"text": text},
		},
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := mcpLoopbackClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("stub: MCP request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var rr struct {
		Error  *rpcError `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &rr); err != nil {
		return fmt.Errorf("stub: decoding MCP response: %w", err)
	}
	if rr.Error != nil {
		return fmt.Errorf("stub: MCP error: %s", rr.Error.Message)
	}
	if rr.Result.IsError {
		msg := "unknown tool error"
		if len(rr.Result.Content) > 0 {
			msg = rr.Result.Content[0].Text
		}
		return fmt.Errorf("stub: %s", msg)
	}
	return nil
}
