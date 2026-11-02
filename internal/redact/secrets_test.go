package redact

import (
	"strings"
	"testing"
)

func TestScrubberMasksAValueSplitAcrossWrites(t *testing.T) {
	t.Parallel()

	secrets := &Secrets{}
	secrets.Add("hunter2-is-the-password")

	var out strings.Builder

	scrubber := secrets.Writer(&out)

	for _, piece := range []string{"token=hunter2-", "is-the-", "password done\n"} {
		_, err := scrubber.Write([]byte(piece))
		if err != nil {
			t.Fatal(err)
		}
	}

	err := scrubber.Flush()
	if err != nil {
		t.Fatal(err)
	}

	if got := out.String(); got != "token=*** done\n" {
		t.Errorf("scrubbed = %q, want %q", got, "token=*** done\n")
	}
}

func TestSecretsMaskEachLineOfAMultiLineValue(t *testing.T) {
	t.Parallel()

	secrets := &Secrets{}
	secrets.Add("-----BEGIN KEY-----\nAAAAsecretbody\n-----END KEY-----")

	got := secrets.String("line two alone: AAAAsecretbody")
	if strings.Contains(got, "AAAAsecretbody") {
		t.Errorf("a line of a multi-line secret survived: %q", got)
	}
}

func TestNilSecretsPassThrough(t *testing.T) {
	t.Parallel()

	var secrets *Secrets

	var out strings.Builder

	scrubber := secrets.Writer(&out)

	_, _ = scrubber.Write([]byte("plain"))
	_ = scrubber.Flush()

	if out.String() != "plain" || secrets.String("plain") != "plain" {
		t.Errorf("a nil Secrets changed the text: %q", out.String())
	}
}

// Only a tail that could still become a value waits; anything else reaches the stream at once, so a step that prints a line and goes quiet is not hidden from the live view.
func TestScrubberHoldsBackOnlyAPossibleValue(t *testing.T) {
	t.Parallel()

	secrets := &Secrets{}
	secrets.Add("hunter2-is-the-password")

	var out strings.Builder

	scrubber := secrets.Writer(&out)

	_, _ = scrubber.Write([]byte("building...\n"))

	if out.String() != "building...\n" {
		t.Errorf("a line with no part of a value was held back: %q", out.String())
	}

	_, _ = scrubber.Write([]byte("next hunt"))

	if out.String() != "building...\nnext " {
		t.Errorf("after a possible start of a value, the stream = %q, want it held from hunt", out.String())
	}
}
