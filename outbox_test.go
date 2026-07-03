package main

import "testing"

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

	good := map[string]Descriptor{
		"text plain": {Kind: KindText, Text: "hi"},
		"text html":  {Kind: KindText, Text: "<b>hi</b>", Format: FormatHTML},
		"code":       {Kind: KindCode, Code: "x", Language: "go"},
		"document":   {Kind: KindDocument, Path: "/abs/x.pdf"},
	}
	for name, d := range good {
		t.Run(name, func(t *testing.T) {
			if err := d.validate(); err != nil {
				t.Errorf("validate(%+v) = %v, want nil", d, err)
			}
		})
	}
}
