package events

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestCleanFactsKeepsResourceOutputFromDrivingTheReader: a fact is printed on a terminal and drawn in a page, so an escape sequence would recolor or retitle the terminal, a newline would forge a line, and a bidi override would reverse what a link appears to say.
func TestCleanFactsKeepsResourceOutputFromDrivingTheReader(t *testing.T) {
	t.Parallel()

	got := CleanFacts([]Fact{
		{Name: "url\x1b]0;owned\x07", Value: "https://x/\x1b[31mred\x1b[0m"},
		{Name: "title", Value: "line one\nok: forged\r"},
		{Name: "author", Value: "a" + string(rune(0x202e)) + "gnp.exe"},
		{Name: "  ", Value: "nameless"},
		{Name: "bad", Value: "\xff\xfe"},
	})

	want := []Fact{
		{Name: "url]0;owned", Value: "https://x/[31mred[0m"},
		{Name: "title", Value: "line one ok: forged"},
		{Name: "author", Value: "agnp.exe"},
		{Name: "bad", Value: "�"},
	}

	if len(got) != len(want) {
		t.Fatalf("CleanFacts = %q, want %q", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fact %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCleanFactsBoundsWhatOneResourceHandsBack(t *testing.T) {
	t.Parallel()

	many := make([]Fact, 100)
	for i := range many {
		many[i] = Fact{Name: strings.Repeat("n", 500), Value: strings.Repeat("€", 5000)}
	}

	got := CleanFacts(many)
	if len(got) != 64 {
		t.Errorf("kept %d facts, want 64", len(got))
	}

	if len(got[0].Name) != 128 || len(got[0].Value) != 4095 {
		t.Errorf("a fact is %d/%d bytes, want its name cut to 128 and its value to the last whole character within 4096", len(got[0].Name), len(got[0].Value))
	}

	if !strings.HasSuffix(got[0].Value, "€") {
		t.Error("a value was cut through a character")
	}
}

func TestFactLinkIsOnlyAnHTTPAddress(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]bool{
		"https://github.com/acme/app/pull/1#issuecomment-2": true,
		"HTTP://example.com/x":                              true,
		"javascript:alert(1)":                               false,
		"JavaScript://example.com/%0A":                      false,
		"data:text/html,<script>":                           false,
		"//example.com/x":                                   false,
		"/relative/path":                                    false,
		"ftp://example.com/x":                               false,
		"https://example.com/a b":                           false,
		`https://example.com/"><b>`:                         false,
		"plain words":                                       false,
	} {
		if _, got := FactLink(value); got != want {
			t.Errorf("FactLink(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestDecodeFactsReadsWhatEncodeFactsKept(t *testing.T) {
	t.Parallel()

	facts := []Fact{{Name: "url", Value: "https://x/1"}, {Name: "title", Value: "T"}}

	got := DecodeFacts(EncodeFacts(facts))
	if len(got) != 2 || got[0] != facts[0] || got[1] != facts[1] {
		t.Errorf("round trip = %q, want %q", got, facts)
	}

	if EncodeFacts(nil) != "" || DecodeFacts("") != nil || DecodeFacts("{not json") != nil {
		t.Error("none must encode to nothing, and nothing readable decode to none")
	}

	if got := DecodeFacts(`[{"name":"x","value":"a\u001b[2Jb"}]`); len(got) != 1 || got[0].Value != "a[2Jb" {
		t.Errorf("a kept fact is cleaned on the way out too: %q", got)
	}
}

// TestAFactWithNoBusIsPrintedAsNameAndValue: with nothing observing, a fact is a line like any note, and it says which fact it is.
func TestAFactWithNoBusIsPrintedAsNameAndValue(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	ctx := WithOutput(context.Background(), Output{Stdout: &out, Stderr: &out})
	FactNote(ctx, Fact{Name: "url", Value: "https://x/1"}, true)

	if got := out.String(); got != "url: https://x/1\n" {
		t.Errorf("printed %q, want the fact's name and value", got)
	}
}
