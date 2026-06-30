package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// readOnlyDescriptor reads the single descriptor expected in dir.
func readOnlyDescriptor(t *testing.T, dir string) (Descriptor, string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
		if strings.HasSuffix(e.Name(), ".tmp") || strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover temp file in outbox: %q", e.Name())
		}
	}
	if len(names) != 1 {
		t.Fatalf("expected exactly one descriptor, got %v", names)
	}
	b, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var d Descriptor
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("unmarshal %s: %v", names[0], err)
	}
	return d, names[0]
}

func TestDropRoundTrip(t *testing.T) {
	cases := map[string]Descriptor{
		"text-plain": {Kind: KindText, Text: "hello"},
		"text-html":  {Kind: KindText, Text: "<b>hi</b>", Format: FormatHTML},
		"code":       {Kind: KindCode, Code: "package main", Language: "go", Caption: "main.go"},
		"document":   {Kind: KindDocument, Path: "/abs/report.pdf", Filename: "report.pdf"},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := want.Drop(dir); err != nil {
				t.Fatalf("Drop: %v", err)
			}
			got, _ := readOnlyDescriptor(t, dir)
			if got.V != descriptorVersion {
				t.Errorf("V = %d, want %d", got.V, descriptorVersion)
			}
			want.V = descriptorVersion
			if got != want {
				t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestDropOrderSorts(t *testing.T) {
	dir := t.TempDir()
	const n = 5
	var dropped []string
	for i := 0; i < n; i++ {
		p, err := (&Descriptor{Kind: KindText, Text: "m"}).Drop(dir)
		if err != nil {
			t.Fatalf("Drop %d: %v", i, err)
		}
		dropped = append(dropped, filepath.Base(p))
	}
	ents, err := os.ReadDir(dir) // ReadDir returns names sorted
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var onDisk []string
	for _, e := range ents {
		onDisk = append(onDisk, e.Name())
	}
	if !sort.StringsAreSorted(dropped) {
		t.Errorf("drop order is not lexically sorted: %v", dropped)
	}
	if strings.Join(onDisk, ",") != strings.Join(dropped, ",") {
		t.Errorf("on-disk order %v != drop order %v", onDisk, dropped)
	}
}

func TestValidate(t *testing.T) {
	bad := map[string]Descriptor{
		"no kind":        {Text: "x"},
		"unknown kind":   {Kind: "photo", Text: "x"},
		"empty text":     {Kind: KindText},
		"bad format":     {Kind: KindText, Text: "x", Format: "markdown"},
		"empty code":     {Kind: KindCode},
		"empty doc path": {Kind: KindDocument},
	}
	for name, d := range bad {
		t.Run(name, func(t *testing.T) {
			if err := d.validate(); err == nil {
				t.Errorf("validate(%+v) = nil, want error", d)
			}
		})
	}
}
