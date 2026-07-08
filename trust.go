package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// markProjectTrusted sets hasTrustDialogAccepted=true for projectPath in the
// operator's ~/.claude.json, so the responder's static cwd is a trusted Claude
// Code workspace — the project permissions.allow is honored and, on a vanilla
// build, Grep/Glob are registered. Trust is keyed by path, so this survives the
// per-start reset+regeneration of the project's contents.
//
// It is a read-modify-write that preserves every other key in ~/.claude.json (the
// file holds the operator's whole Claude Code config, including sibling projects):
// every level is decoded as json.RawMessage so untouched values — notably large
// integer fields — are re-emitted byte-for-byte, never round-tripped through
// float64. It is a no-op when the project is already trusted, which avoids churn
// and shrinks the window of the read-modify-write race with a concurrently running
// claude. A missing ~/.claude.json is created from an empty object.
func markProjectTrusted(projectPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locating home dir: %w", err)
	}
	// Claude Code keys .projects by the canonical (symlink-resolved) path; match its
	// canonicalization so we set trust on the same entry claude reads. EvalSymlinks
	// needs the path to exist — the caller creates $workdir/project first.
	key := projectPath
	if resolved, err := filepath.EvalSymlinks(projectPath); err == nil {
		key = resolved
	}
	return setProjectTrust(filepath.Join(home, ".claude.json"), key)
}

// setProjectTrust is the read-modify-write half of markProjectTrusted: it sets
// hasTrustDialogAccepted=true for the project keyed by key in the JSON file at
// configPath, preserving every other key. It is split out (from the $HOME +
// canonical-path resolution) so it is unit-testable against a temp file. See
// markProjectTrusted for the byte-preservation and no-op rationale.
func setProjectTrust(configPath, key string) error {
	root := map[string]json.RawMessage{}
	if b, err := os.ReadFile(configPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("reading %s: %w", configPath, err)
		}
		// Absent: start from an empty object.
	} else if err := json.Unmarshal(b, &root); err != nil {
		return fmt.Errorf("parsing %s: %w", configPath, err)
	}
	if root == nil {
		root = map[string]json.RawMessage{}
	}

	projects := map[string]json.RawMessage{}
	if raw, ok := root["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return fmt.Errorf("parsing projects in %s: %w", configPath, err)
		}
	}
	if projects == nil {
		projects = map[string]json.RawMessage{}
	}

	entry := map[string]json.RawMessage{}
	if raw, ok := projects[key]; ok {
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("parsing project %q in %s: %w", key, configPath, err)
		}
	}
	if entry == nil {
		entry = map[string]json.RawMessage{}
	}

	// No-op if already trusted.
	if raw, ok := entry["hasTrustDialogAccepted"]; ok {
		var trusted bool
		if err := json.Unmarshal(raw, &trusted); err == nil && trusted {
			return nil
		}
	}
	entry["hasTrustDialogAccepted"] = json.RawMessage("true")

	// Re-marshal preserving all other keys at every level (sibling projects and
	// unrelated top-level keys stay as opaque RawMessage blobs).
	entryRaw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	projects[key] = entryRaw
	projectsRaw, err := json.Marshal(projects)
	if err != nil {
		return err
	}
	root["projects"] = projectsRaw
	compact, err := json.Marshal(root)
	if err != nil {
		return err
	}
	// Pretty-print for a human-readable ~/.claude.json. json.Indent (not
	// json.MarshalIndent) is deliberate: root's values are RawMessage blobs, and
	// MarshalIndent re-emits RawMessage verbatim rather than re-indenting it,
	// yielding ragged nesting. json.Indent re-indents an already-valid JSON stream
	// uniformly and never rewrites value bytes, so large integers stay byte-exact.
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return err
	}

	// Atomic write (temp + rename, 0600), mirroring Sessions.persist in internal/store.
	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, configPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
