// Package policy is the persona-policy engine: the built-in fragment catalog,
// selector validation vocabulary, the axis-based conflict/eviction rules, and
// the composition of the persona text the dispatcher injects into the responder
// at spawn (--append-system-prompt). The built-in fragments are embedded next
// to the code, so the whole persona-resolution surface — what a selector can
// mean and what text it composes to — lives and is audited in one place.
package policy

import (
	"embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Default is the persona composed when none is configured: the scoped FAQ
// that declines off-topic.
const Default = "normal"

// BuiltinOrder lists the persona fragments shipped in policies/, in catalog
// order — the single source of truth for the built-in set. It backs IsBuiltin
// (membership), the `--policy help` catalog, and the "built-in: …" hints in
// error messages, so a new fragment is added in exactly one place. The
// refusal-stance trio (normal/norefuse/strict) all carry `axis: refusal` in
// their frontmatter, so at most one may appear in a resolved persona (see
// CheckAxisConflicts); introspect and outbox-rw are axis-less and purely
// additive.
var BuiltinOrder = []string{"normal", "norefuse", "strict", "introspect", "outbox-rw"}

// fragments holds the built-in persona fragments. Embedded here (not in the
// scaffold's asset tree) so the engine is self-contained.
//
//go:embed policies
var fragments embed.FS

// builtins is the membership set derived from BuiltinOrder.
var builtins = func() map[string]bool {
	m := make(map[string]bool, len(BuiltinOrder))
	for _, p := range BuiltinOrder {
		m[p] = true
	}
	return m
}()

// IsBuiltin reports whether a policy selector names a built-in fragment.
func IsBuiltin(policy string) bool {
	return builtins[policy]
}

// IsPath reports whether a policy selector names a custom fragment FILE
// (rather than a built-in): anything containing a path separator or ending in .md.
func IsPath(policy string) bool {
	return strings.ContainsRune(policy, filepath.Separator) || strings.HasSuffix(policy, ".md")
}

// readRaw returns the raw bytes for a policy selector: a built-in name reads
// policies/<name>.md from the embed; anything else is a path to a custom
// fragment file read from disk. Empty selects Default. The bytes may carry a
// leading `axis:` frontmatter block — use parseFragment to split it off.
func readRaw(policy string) ([]byte, error) {
	if policy == "" {
		policy = Default
	}
	if IsPath(policy) {
		data, err := os.ReadFile(policy)
		if err != nil {
			return nil, fmt.Errorf("reading custom policy %s: %w", policy, err)
		}
		return data, nil
	}
	if !builtins[policy] {
		return nil, fmt.Errorf("unknown policy %q (built-in: %s; or a path to a .md fragment)", policy, strings.Join(BuiltinOrder, ", "))
	}
	return fragments.ReadFile("policies/" + policy + ".md")
}

// parseFragment splits a policy fragment into its frontmatter fields (empty map if
// none) and its body with the frontmatter removed. Frontmatter is an OPT-IN leading
// `---` … `---` block of `key: value` lines; we read `axis` (the mutual-exclusion
// guard, so two fragments sharing a non-empty axis cannot co-exist in one resolved
// persona) and `summary` (the one-line gloss shown by `--policy help`). A fragment
// with no leading fence (or no closing fence) is all body with no fields, so the
// plain "just write an .md" case needs no ceremony. Parsed by hand (no YAML
// dependency): the block is a handful of `key: value` lines.
func parseFragment(data []byte) (fields map[string]string, body []byte) {
	fields = map[string]string{}
	rest, ok := strings.CutPrefix(string(data), "---\n")
	if !ok {
		return fields, data
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fields, data // no closing fence — treat the whole thing as body
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	// Body is everything past the closing fence line.
	return fields, []byte(strings.TrimPrefix(rest[end+len("\n---"):], "\n"))
}

// axisOf returns the axis a policy selector declares (empty if none).
func axisOf(policy string) (string, error) {
	raw, err := readRaw(policy)
	if err != nil {
		return "", err
	}
	fields, _ := parseFragment(raw)
	return fields["axis"], nil
}

// summaryOf returns the one-line `summary:` a policy selector declares (empty if
// none). It backs the `--policy help` catalog.
func summaryOf(policy string) (string, error) {
	raw, err := readRaw(policy)
	if err != nil {
		return "", err
	}
	fields, _ := parseFragment(raw)
	return fields["summary"], nil
}

// PrintCatalog writes the built-in policy catalog — each name aligned with its
// `summary:` gloss, in BuiltinOrder — followed by a note on custom fragments and
// composition. It backs `--policy help`. The summaries come from the embed, so a read
// error is unexpected but surfaced rather than swallowed.
func PrintCatalog(w io.Writer) error {
	width := 0
	for _, p := range BuiltinOrder {
		if len(p) > width {
			width = len(p)
		}
	}
	fmt.Fprintln(w, "built-in policies (persona fragments; the default is `normal`):")
	fmt.Fprintln(w)
	for _, p := range BuiltinOrder {
		summary, err := summaryOf(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %-*s  %s\n", width, p, summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "A --policy value may also be a path to your own .md fragment. --policy is")
	fmt.Fprintln(w, "repeatable and additive: entries merge in order into one persona (the refusal")
	fmt.Fprintln(w, "trio normal/norefuse/strict are mutually exclusive; the rest are additive).")
	return nil
}

// Load merges the persona-fragment BODIES for a list of selectors into a
// single fragment: each is read, its frontmatter stripped, trimmed of surrounding
// blank lines, and joined in order with a blank line between them, so several
// stances (built-in names and/or custom paths) layer into one persona. An empty
// list selects Default — the single-selector case is just one element.
func Load(policies []string) ([]byte, error) {
	if len(policies) == 0 {
		policies = []string{Default}
	}
	parts := make([]string, 0, len(policies))
	for _, p := range policies {
		raw, err := readRaw(p)
		if err != nil {
			return nil, err
		}
		_, body := parseFragment(raw)
		parts = append(parts, strings.TrimSpace(string(body)))
	}
	return []byte(strings.Join(parts, "\n\n")), nil
}

// CheckAxisConflicts reports an error if two selectors in the list declare the
// same non-empty axis — the opt-in mutual-exclusion guard. It runs at config load
// over the default set and over each per-user override list, so a contradictory
// pairing (e.g. norefuse + strict) fails fast at startup, not mid-run.
func CheckAxisConflicts(policies []string) error {
	seen := make(map[string]string, len(policies))
	for _, p := range policies {
		axis, err := axisOf(p)
		if err != nil {
			return err
		}
		if axis == "" {
			continue
		}
		if prev, ok := seen[axis]; ok {
			return fmt.Errorf("policies %q and %q both declare axis %q — only one per axis", prev, p, axis)
		}
		seen[axis] = p
	}
	return nil
}

// WithDefaultStance ensures the resolved persona has a fragment on Default's
// axis — the "refusal" axis (normal/norefuse/strict). Axis-less fragments (introspect,
// outbox-rw, or a plain custom .md) are MODIFIERS meant to layer on top of a base
// stance; a list of only those — e.g. a lone `--policy ./my-rw.md` — would otherwise
// leave the agent with no base FAQ stance at all. When no fragment claims that axis,
// Default (normal) is prepended as the base, generalizing the empty-list
// fallback. A custom fragment can occupy the slot itself by declaring `axis: refusal`,
// which suppresses the injection (the escape hatch for a deliberately base-less
// persona). An empty list is returned unchanged — Load maps it to
// Default on its own.
func WithDefaultStance(policies []string) ([]string, error) {
	if len(policies) == 0 {
		return policies, nil
	}
	base, err := axisOf(Default)
	if err != nil {
		return nil, err
	}
	if base == "" {
		return policies, nil // Default declares no axis — nothing to floor
	}
	for _, p := range policies {
		axis, err := axisOf(p)
		if err != nil {
			return nil, err
		}
		if axis == base {
			return policies, nil // the axis is already occupied
		}
	}
	return append([]string{Default}, policies...), nil
}

// ResolveEffective layers a per-user override list on top of the default
// list along axes: an override fragment that declares an axis EVICTS the default
// fragment on that same axis (replacing it in place); an axis-less override (or one
// whose axis no default carries) is appended. So a default of {strict, rw} with a
// user override of {norefuse} yields {norefuse, rw} — norefuse displaces strict on
// the refusal axis, rw is untouched. The override list is assumed already free of
// internal axis conflicts (checked at load).
func ResolveEffective(base, override []string) ([]string, error) {
	result := append([]string(nil), base...)
	axisAt := make(map[string]int) // axis -> index in result
	for i, p := range result {
		axis, err := axisOf(p)
		if err != nil {
			return nil, err
		}
		if axis != "" {
			axisAt[axis] = i
		}
	}
	for _, o := range override {
		axis, err := axisOf(o)
		if err != nil {
			return nil, err
		}
		if i, ok := axisAt[axis]; axis != "" && ok {
			result[i] = o // evict the default fragment on this axis
			continue
		}
		result = append(result, o)
		if axis != "" {
			axisAt[axis] = len(result) - 1
		}
	}
	return result, nil
}
