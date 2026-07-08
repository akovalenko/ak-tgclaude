package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// composedPersona returns the persona text Load composes for the given
// selectors — exactly what the dispatcher injects via --append-system-prompt.
func composedPersona(t *testing.T, policies ...string) string {
	t.Helper()
	b, err := Load(policies)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestComposePolicyText(t *testing.T) {
	// "" defaults to normal; each selector composes to its distinctive persona text.
	def := composedPersona(t, "")
	nr := composedPersona(t, "norefuse")
	intro := composedPersona(t, "introspect")
	// Frontmatter (axis:) is stripped from the composed text.
	for _, p := range []string{def, nr, intro} {
		if strings.Contains(p, "axis:") || strings.HasPrefix(p, "---") {
			t.Errorf("frontmatter leaked into composed persona:\n%s", p)
		}
	}
	// normal (the default) declines off-topic and carries the untrusted-input framing.
	if !strings.Contains(def, "out of scope") || !strings.Contains(def, "untrusted") {
		t.Errorf("normal policy should scope + treat input as untrusted:\n%s", def)
	}
	// norefuse says not to decline and drops the untrusted framing.
	if !strings.Contains(nr, "NOT** decline") {
		t.Errorf("norefuse policy should say not to decline:\n%s", nr)
	}
	if strings.Contains(nr, "untrusted") {
		t.Errorf("norefuse policy should not carry the untrusted-input framing:\n%s", nr)
	}
	// introspect is the candid/debug persona.
	if !strings.Contains(intro, "introspect") || !strings.Contains(intro, "precise") {
		t.Errorf("introspect policy should be the candid/debug persona:\n%s", intro)
	}
}

func TestComposePolicyCustomFile(t *testing.T) {
	// A --policy path composes an operator's own fragment.
	f := filepath.Join(t.TempDir(), "my-policy.md")
	if err := os.WriteFile(f, []byte("You are a CUSTOM persona for this bot.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if body := composedPersona(t, f); !strings.Contains(body, "You are a CUSTOM persona for this bot.") {
		t.Errorf("custom policy fragment not composed in:\n%s", body)
	}
	// An unknown built-in NAME (not a path) is an error, not a silent miss.
	if _, err := Load([]string{"bogus"}); err == nil {
		t.Errorf("unknown policy name should error")
	}
}

func TestComposeMergesPolicies(t *testing.T) {
	// Several selectors merge in order into ONE persona: both fragments' distinctive
	// prose is present, the marker is gone, and a custom .md layers on top of a
	// built-in.
	f := filepath.Join(t.TempDir(), "extra.md")
	if err := os.WriteFile(f, []byte("EXTRA persona layered on top.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := composedPersona(t, "norefuse", f)
	if !strings.Contains(body, "NOT** decline") {
		t.Errorf("merged policy dropped the norefuse fragment:\n%s", body)
	}
	if !strings.Contains(body, "EXTRA persona layered on top.") {
		t.Errorf("merged policy dropped the custom fragment:\n%s", body)
	}
	// A blank line separates the two fragments (norefuse body then the custom one).
	if !strings.Contains(body, "\n\nEXTRA persona layered on top.") {
		t.Errorf("merged fragments not blank-line separated:\n%s", body)
	}
}

func TestParseFragment(t *testing.T) {
	// Frontmatter is stripped; axis and summary are read into the fields map.
	fields, body := parseFragment([]byte("---\naxis: refusal\nsummary: a scoped FAQ\n---\nYou are strict.\n"))
	if fields["axis"] != "refusal" {
		t.Errorf("axis = %q, want refusal", fields["axis"])
	}
	if fields["summary"] != "a scoped FAQ" {
		t.Errorf("summary = %q, want %q", fields["summary"], "a scoped FAQ")
	}
	if strings.TrimSpace(string(body)) != "You are strict." {
		t.Errorf("body = %q, want the persona text without frontmatter", body)
	}
	// No frontmatter => empty fields, the whole thing is body.
	if f, b := parseFragment([]byte("Just a persona.\n")); f["axis"] != "" || strings.TrimSpace(string(b)) != "Just a persona." {
		t.Errorf("plain fragment: axis=%q body=%q", f["axis"], b)
	}
	// A leading fence with no closing fence is all body (no panic, no fields).
	if f, _ := parseFragment([]byte("---\nnot really frontmatter\n")); f["axis"] != "" {
		t.Errorf("unterminated frontmatter should yield no axis, got %q", f["axis"])
	}
	// A quoted axis value is unquoted.
	if f, _ := parseFragment([]byte("---\naxis: \"refusal\"\n---\nx")); f["axis"] != "refusal" {
		t.Errorf("quoted axis = %q, want refusal", f["axis"])
	}
}

func TestPolicySummary(t *testing.T) {
	// Every built-in ships a non-empty summary (backs `--policy help`).
	for _, p := range BuiltinOrder {
		s, err := summaryOf(p)
		if err != nil {
			t.Fatalf("summaryOf(%q): %v", p, err)
		}
		if strings.TrimSpace(s) == "" {
			t.Errorf("built-in %q has no summary:", p)
		}
	}
	// A fragment without a summary field yields "".
	f := filepath.Join(t.TempDir(), "no-summary.md")
	if err := os.WriteFile(f, []byte("You are a custom persona.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s, _ := summaryOf(f); s != "" {
		t.Errorf("summary of a fragment without one = %q, want empty", s)
	}
}

func TestPrintPolicyCatalog(t *testing.T) {
	var sb strings.Builder
	if err := PrintCatalog(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	// Every built-in name and its summary appear, in order, plus the custom-fragment note.
	last := -1
	for _, p := range BuiltinOrder {
		i := strings.Index(out, p)
		if i < 0 {
			t.Errorf("catalog missing policy %q:\n%s", p, out)
		}
		if i < last {
			t.Errorf("catalog lists %q out of BuiltinOrder:\n%s", p, out)
		}
		last = i
		s, _ := summaryOf(p)
		if !strings.Contains(out, s) {
			t.Errorf("catalog missing summary for %q:\n%s", p, out)
		}
	}
	if !strings.Contains(out, "path to your own .md fragment") {
		t.Errorf("catalog should mention custom fragments:\n%s", out)
	}
}

func TestOutboxRWPolicy(t *testing.T) {
	// outbox-rw is a recognized built-in, axis-less (additive), and composes its
	// distinctive outbox/clone guidance.
	if !IsBuiltin("outbox-rw") {
		t.Fatal("outbox-rw should be a built-in policy")
	}
	if axis, err := axisOf("outbox-rw"); err != nil || axis != "" {
		t.Errorf("outbox-rw axis = %q err=%v, want axis-less", axis, err)
	}
	// Axis-less => it stacks on any refusal stance without a conflict.
	if err := CheckAxisConflicts([]string{"strict", "outbox-rw"}); err != nil {
		t.Errorf("strict + outbox-rw should not conflict: %v", err)
	}
	body := composedPersona(t, "strict", "outbox-rw")
	if !strings.Contains(body, "outbox") || !strings.Contains(body, "git clone --shared") {
		t.Errorf("outbox-rw persona missing its outbox/clone guidance:\n%s", body)
	}
}

func TestWithDefaultStance(t *testing.T) {
	// An axis-less-only list gets normal prepended as the base stance.
	for _, name := range []string{"introspect", "outbox-rw"} {
		got, err := WithDefaultStance([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "normal" || got[1] != name {
			t.Errorf("WithDefaultStance([%s]) = %v, want [normal %s]", name, got, name)
		}
	}
	// A list that already carries a refusal-axis fragment is left untouched.
	for _, in := range [][]string{{"strict"}, {"norefuse"}, {"normal"}, {"strict", "outbox-rw"}} {
		got, err := WithDefaultStance(in)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(in) || got[0] != in[0] {
			t.Errorf("WithDefaultStance(%v) = %v, want unchanged", in, got)
		}
	}
	// An empty list is returned unchanged (Load maps it to Default).
	if got, _ := WithDefaultStance(nil); len(got) != 0 {
		t.Errorf("WithDefaultStance(nil) = %v, want empty", got)
	}
	// A custom fragment declaring axis: refusal occupies the slot — no floor.
	fr := filepath.Join(t.TempDir(), "myrefusal.md")
	if err := os.WriteFile(fr, []byte("---\naxis: refusal\n---\nMy base.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := WithDefaultStance([]string{fr})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != fr {
		t.Errorf("axis:refusal custom = %v, want [%q] (no normal floor)", got, fr)
	}
}

func TestCheckAxisConflicts(t *testing.T) {
	// Two refusal-axis built-ins conflict.
	if err := CheckAxisConflicts([]string{"normal", "norefuse"}); err == nil {
		t.Errorf("normal + norefuse should conflict on axis refusal")
	}
	// One refusal + an axis-less one is fine.
	if err := CheckAxisConflicts([]string{"strict", "introspect"}); err != nil {
		t.Errorf("strict + introspect should not conflict: %v", err)
	}
	// A single fragment never conflicts.
	if err := CheckAxisConflicts([]string{"norefuse"}); err != nil {
		t.Errorf("single fragment should not conflict: %v", err)
	}
}

func TestResolveEffective(t *testing.T) {
	// An override on the shared axis EVICTS the default fragment in place; an
	// axis-less default (introspect) is untouched.
	got, err := ResolveEffective([]string{"strict", "introspect"}, []string{"norefuse"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "norefuse" || got[1] != "introspect" {
		t.Errorf("eviction in place = %v, want [norefuse introspect]", got)
	}
	// An axis-less override just appends (nothing to evict).
	if got, _ := ResolveEffective([]string{"strict"}, []string{"introspect"}); len(got) != 2 || got[0] != "strict" || got[1] != "introspect" {
		t.Errorf("axis-less append = %v, want [strict introspect]", got)
	}
	// An empty override yields the base unchanged.
	if got, _ := ResolveEffective([]string{"strict"}, nil); len(got) != 1 || got[0] != "strict" {
		t.Errorf("empty override = %v, want [strict]", got)
	}
}
