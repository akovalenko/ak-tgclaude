package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestMCP(t *testing.T, sender Sender) *mcpServer {
	t.Helper()
	m, err := newMCPServer(sender, "test", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// postRPC POSTs a JSON-RPC message to the server and returns the decoded
// response (nil for a notification's empty 202 body).
func postRPC(t *testing.T, url, token string, req map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, b)
	}
	return out
}

func callSendMessage(t *testing.T, m *mcpServer, token, text string, html bool) map[string]any {
	return postRPC(t, m.URL(), token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      toolSendMessage,
			"arguments": map[string]any{"text": text, "html": html},
		},
	})
}

func isToolError(resp map[string]any) bool {
	res, ok := resp["result"].(map[string]any)
	if !ok {
		return resp["error"] != nil
	}
	e, _ := res["isError"].(bool)
	return e
}

func toolErrorText(resp map[string]any) string {
	res, _ := resp["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	c0, _ := content[0].(map[string]any)
	s, _ := c0["text"].(string)
	return s
}

func TestMCPToolsCallDelivers(t *testing.T) {
	f := &fakeSender{}
	m := newTestMCP(t, f)
	tok, err := m.Register(Route{ChatID: 42, ReplyTo: 7}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if resp := callSendMessage(t, m, tok, "hi", true); isToolError(resp) {
		t.Fatalf("unexpected tool error: %v", resp)
	}
	calls := f.snapshot()
	if len(calls) != 1 || calls[0].text != "hi" || calls[0].mode != "HTML" {
		t.Fatalf("delivery wrong: %+v", calls)
	}
	if calls[0].route.ChatID != 42 || calls[0].route.ReplyTo != 7 {
		t.Errorf("route not resolved from token: %+v", calls[0].route)
	}
}

func TestMCPDeliveredCount(t *testing.T) {
	m := newTestMCP(t, &fakeSender{})
	tok, err := m.Register(Route{ChatID: 5, ReplyTo: 2}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if n := m.DeliveredCount(tok); n != 0 {
		t.Fatalf("fresh invocation delivered=%d, want 0", n)
	}
	if resp := callSendMessage(t, m, tok, "one", false); isToolError(resp) {
		t.Fatalf("send failed: %v", resp)
	}
	if resp := callSendMessage(t, m, tok, "two", false); isToolError(resp) {
		t.Fatalf("send failed: %v", resp)
	}
	if n := m.DeliveredCount(tok); n != 2 {
		t.Fatalf("after two sends delivered=%d, want 2", n)
	}
	// An unknown token reports 0 rather than panicking.
	if n := m.DeliveredCount("bogus"); n != 0 {
		t.Fatalf("unknown token delivered=%d, want 0", n)
	}
}

func TestMCPDeliveredCountSkipsProgress(t *testing.T) {
	f := &fakeSender{}
	m := newTestMCP(t, f)
	tok, err := m.Register(Route{ChatID: 5}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A progress note is delivered but must NOT count toward the delivery tally.
	resp := postRPC(t, m.URL(), tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      toolSendMessage,
			"arguments": map[string]any{"text": "working...", "progress": true},
		},
	})
	if isToolError(resp) {
		t.Fatalf("progress send failed: %v", resp)
	}
	if len(f.snapshot()) != 1 {
		t.Fatalf("progress note should still be delivered, got %d messages", len(f.snapshot()))
	}
	if n := m.DeliveredCount(tok); n != 0 {
		t.Fatalf("progress send must NOT count: delivered=%d, want 0", n)
	}
	// A normal send counts.
	if resp := callSendMessage(t, m, tok, "the answer", false); isToolError(resp) {
		t.Fatalf("send failed: %v", resp)
	}
	if n := m.DeliveredCount(tok); n != 1 {
		t.Fatalf("normal send after a progress note should count: delivered=%d, want 1", n)
	}
}

func TestMCPDeliveredCountIgnoresFailedSend(t *testing.T) {
	// A send Telegram rejects must NOT count as delivered — else the guard would
	// think a dropped answer was delivered and never re-prompt.
	m := newTestMCP(t, &fakeSender{err: errors.New("telegram rejected")})
	tok, err := m.Register(Route{ChatID: 5}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if resp := callSendMessage(t, m, tok, "hi", false); !isToolError(resp) {
		t.Fatal("expected a tool error on a failing send")
	}
	if n := m.DeliveredCount(tok); n != 0 {
		t.Fatalf("failed send must not count: delivered=%d, want 0", n)
	}
}

func TestMCPUnauthorized(t *testing.T) {
	f := &fakeSender{}
	m := newTestMCP(t, f)
	// No token registered: an unknown bearer is rejected and nothing is delivered.
	resp := callSendMessage(t, m, "bogus", "hi", false)
	if resp["error"] == nil {
		t.Fatalf("expected an unauthorized JSON-RPC error, got %v", resp)
	}
	if len(f.snapshot()) != 0 {
		t.Error("unauthorized call must not deliver")
	}
}

func TestMCPUnregisterInvalidates(t *testing.T) {
	f := &fakeSender{}
	m := newTestMCP(t, f)
	tok, _ := m.Register(Route{ChatID: 1}, t.TempDir())
	m.Unregister(tok)
	if resp := callSendMessage(t, m, tok, "hi", false); resp["error"] == nil {
		t.Fatalf("call after Unregister must fail, got %v", resp)
	}
	if len(f.snapshot()) != 0 {
		t.Error("call after Unregister must not deliver")
	}
}

func TestMCPInitializeEchoesVersionAndLists(t *testing.T) {
	m := newTestMCP(t, &fakeSender{})
	tok, _ := m.Register(Route{ChatID: 1}, t.TempDir())

	init := postRPC(t, m.URL(), tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-03-26"},
	})
	res, _ := init["result"].(map[string]any)
	if res == nil || res["protocolVersion"] != "2025-03-26" {
		t.Errorf("initialize should echo the requested protocol version: %v", init)
	}
	if info, _ := res["serverInfo"].(map[string]any); info == nil || info["name"] != "ak-tgclaude" {
		t.Errorf("initialize should carry serverInfo: %v", res)
	}

	list := postRPC(t, m.URL(), tok, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	lres, _ := list["result"].(map[string]any)
	tools, _ := lres["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("want 3 tools, got %d: %v", len(tools), lres)
	}
}

func TestMCPNotificationGetsNoBody(t *testing.T) {
	m := newTestMCP(t, &fakeSender{})
	tok, _ := m.Register(Route{ChatID: 1}, t.TempDir())
	if resp := postRPC(t, m.URL(), tok, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/initialized",
	}); resp != nil {
		t.Errorf("a notification should get no response body, got %v", resp)
	}
}

func TestMCPSendDocumentConfined(t *testing.T) {
	f := &fakeSender{}
	m := newTestMCP(t, f)
	docDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(docDir, "report.pdf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, _ := m.Register(Route{ChatID: 1}, docDir)

	callDoc := func(path string) map[string]any {
		return postRPC(t, m.URL(), tok, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{
				"name":      toolSendDocument,
				"arguments": map[string]any{"path": path},
			},
		})
	}
	// A file in the outbox delivers.
	if resp := callDoc("report.pdf"); isToolError(resp) {
		t.Fatalf("in-dir document should deliver: %v", resp)
	}
	// An absolute host path collapses to a basename that is not in docDir → error.
	if resp := callDoc("/etc/passwd"); !isToolError(resp) {
		t.Errorf("out-of-outbox document must be rejected: %v", resp)
	}
	// A traversal attempt likewise collapses to its basename and is rejected.
	if resp := callDoc("../../secret.txt"); !isToolError(resp) {
		t.Errorf("traversal document path must be rejected: %v", resp)
	}
	calls := f.snapshot()
	if len(calls) != 1 || calls[0].kind != "document" {
		t.Errorf("only the in-outbox document should have been delivered: %+v", calls)
	}
}

func TestMCPToolErrorCarriesTelegramReason(t *testing.T) {
	f := &fakeSender{err: &APIError{Code: 400, Description: "can't parse entities"}}
	m := newTestMCP(t, f)
	tok, _ := m.Register(Route{ChatID: 1}, t.TempDir())
	resp := callSendMessage(t, m, tok, "<b>bad", true)
	if !isToolError(resp) {
		t.Fatalf("a Telegram reject should be a tool error: %v", resp)
	}
	if txt := toolErrorText(resp); !strings.Contains(txt, "can't parse entities") {
		t.Errorf("tool error should carry the Telegram description, got %q", txt)
	}
}

func TestMCPMissingRequiredArg(t *testing.T) {
	f := &fakeSender{}
	m := newTestMCP(t, f)
	tok, _ := m.Register(Route{ChatID: 1}, t.TempDir())
	// send_message with empty text → validation tool error, nothing delivered.
	if resp := callSendMessage(t, m, tok, "", false); !isToolError(resp) {
		t.Errorf("empty text should be a tool error: %v", resp)
	}
	if len(f.snapshot()) != 0 {
		t.Error("an invalid call must not deliver")
	}
}

func TestMCPWrongPath404(t *testing.T) {
	m := newTestMCP(t, &fakeSender{})
	tok, _ := m.Register(Route{ChatID: 1}, t.TempDir())
	// A POST to a path other than /mcp is a plain 404 (and logged) — not treated
	// as JSON-RPC — so a client dialing the wrong URL is visible, not silent.
	base := strings.TrimSuffix(m.URL(), "/mcp")
	req, err := http.NewRequest(http.MethodPost, base+"/wrong", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := mcpLoopbackClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("wrong path should be 404, got %d", resp.StatusCode)
	}
}

func TestBuildMCPConfig(t *testing.T) {
	cfg := buildMCPConfig("http://127.0.0.1:1234/mcp", "tok123")
	var parsed struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	s, ok := parsed.MCPServers[mcpServerName]
	if !ok || s.Type != "http" || s.URL != "http://127.0.0.1:1234/mcp" {
		t.Fatalf("config wrong: %s", cfg)
	}
	if s.Headers["Authorization"] != "Bearer tok123" {
		t.Errorf("auth header wrong: %v", s.Headers)
	}
}
