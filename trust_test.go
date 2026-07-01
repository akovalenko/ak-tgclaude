package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// bigInt is 2^53 + 1 — not representable as a float64, so a config value carried
// through interface{}/float64 would be corrupted. The RawMessage path must keep it
// byte-exact.
const bigInt = "9007199254740993"

func TestSetProjectTrustPreservesEverything(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	orig := `{
		"numStartups": 42,
		"oauthAccount": {"emailAddress": "op@example.com"},
		"projects": {
			"/home/op/other": {"hasTrustDialogAccepted": true, "lastTotalInputTokens": ` + bigInt + `},
			"/home/op/qa/project": {"hasTrustDialogAccepted": false, "allowedTools": ["Read"], "lastCost": ` + bigInt + `}
		}
	}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := setProjectTrust(path, "/home/op/qa/project"); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		NumStartups  int `json:"numStartups"`
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
		Projects map[string]json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, b)
	}

	var target struct {
		Trusted      bool            `json:"hasTrustDialogAccepted"`
		AllowedTools []string        `json:"allowedTools"`
		LastCost     json.RawMessage `json:"lastCost"`
	}
	if err := json.Unmarshal(got.Projects["/home/op/qa/project"], &target); err != nil {
		t.Fatal(err)
	}
	if !target.Trusted {
		t.Error("target project not trusted after setProjectTrust")
	}
	if len(target.AllowedTools) != 1 || target.AllowedTools[0] != "Read" {
		t.Errorf("target allowedTools not preserved: %v", target.AllowedTools)
	}
	if string(target.LastCost) != bigInt {
		t.Errorf("target lastCost big-int corrupted: got %s want %s", target.LastCost, bigInt)
	}

	if got.NumStartups != 42 {
		t.Errorf("numStartups not preserved: %d", got.NumStartups)
	}
	if got.OAuthAccount.EmailAddress != "op@example.com" {
		t.Errorf("oauthAccount not preserved: %q", got.OAuthAccount.EmailAddress)
	}

	var sibling struct {
		Trusted bool            `json:"hasTrustDialogAccepted"`
		Tokens  json.RawMessage `json:"lastTotalInputTokens"`
	}
	if err := json.Unmarshal(got.Projects["/home/op/other"], &sibling); err != nil {
		t.Fatal(err)
	}
	if !sibling.Trusted {
		t.Error("sibling project trust flipped")
	}
	if string(sibling.Tokens) != bigInt {
		t.Errorf("sibling big-int corrupted: got %s want %s", sibling.Tokens, bigInt)
	}
}

func TestSetProjectTrustCreatesAbsentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := setProjectTrust(path, "/home/op/qa/project"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	var got struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Projects["/home/op/qa/project"].Trusted {
		t.Error("absent-file path did not create a trusted project entry")
	}
}

func TestSetProjectTrustNoOpWhenTrusted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	orig := `{"projects":{"/p":{"hasTrustDialogAccepted":true,"keep":"me"}}}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setProjectTrust(path, "/p"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != orig {
		t.Errorf("no-op path rewrote the file:\n got %s\nwant %s", b, orig)
	}
}

func TestSetProjectTrustWritesIndented(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	// Compact single-line input carrying a big-int; the rewrite must come back
	// pretty-printed while keeping the big-int byte-exact.
	orig := `{"projects":{"/p":{"hasTrustDialogAccepted":false,"lastCost":` + bigInt + `}}}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setProjectTrust(path, "/p"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Indented output: a newline followed by a two-space indent, not a single line.
	if !bytes.Contains(b, []byte("\n  ")) {
		t.Errorf("output not indented (want newline + two-space indent):\n%s", b)
	}
	// Indentation must not corrupt the big-int value.
	if !bytes.Contains(b, []byte(bigInt)) {
		t.Errorf("big-int not preserved through indentation:\n%s", b)
	}
}

func TestSetProjectTrustNullProjects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"projects":null,"numStartups":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setProjectTrust(path, "/p"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("invalid JSON after null-projects: %v\n%s", err, b)
	}
	if !got.Projects["/p"].Trusted {
		t.Error("null projects not handled into a trusted entry")
	}
}
