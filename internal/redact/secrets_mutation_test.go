package redact

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A line of exactly minLine bytes is masked on its own, and one byte shorter is structure.
func TestALineOfExactlyMinLineIsMaskedOnItsOwn(t *testing.T) {
	t.Parallel()

	secrets := &Secrets{}
	secrets.Add("first-long-line\nABCDEFGH\nABCDEFG")

	if got := secrets.String("x ABCDEFGH y ABCDEFG z"); got != "x *** y ABCDEFG z" {
		t.Errorf("String = %q, want the 8-byte line masked and the 7-byte one left", got)
	}
}

// The longest value is masked first, whichever order they were added in: masking the shorter one first would leave the longer one's tail showing.
func TestALongerValueIsMaskedBeforeItsOwnPrefix(t *testing.T) {
	t.Parallel()

	for name, order := range map[string][]string{
		"short first": {"tokenabcdefgh", "tokenabcdefgh-and-the-rest"},
		"long first":  {"tokenabcdefgh-and-the-rest", "tokenabcdefgh"},
	} {
		secrets := &Secrets{}
		for _, value := range order {
			secrets.Add(value)
		}

		if got := secrets.String("v=tokenabcdefgh-and-the-rest."); got != "v=***." {
			t.Errorf("%s: String = %q, want the whole longer value masked", name, got)
		}
	}
}

func TestEmptyReportsWhetherThereIsAnythingToMask(t *testing.T) {
	t.Parallel()

	var none *Secrets
	if !none.Empty() {
		t.Error("a nil Secrets is empty")
	}

	secrets := &Secrets{}
	if !secrets.Empty() {
		t.Error("a new Secrets is empty")
	}

	secrets.Add("")
	if !secrets.Empty() {
		t.Error("adding the empty string leaves it empty")
	}

	secrets.Add("something-secret")
	if secrets.Empty() {
		t.Error("a Secrets holding a value is not empty")
	}
}

// Whatever the chunking, the scrubbed stream equals masking the whole text: a value split at any point is still masked, and nothing is lost to the held tail.
func TestScrubberOutputEqualsStringAtEverySplit(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		values []string
		text   string
	}{
		"two unrelated values":            {[]string{"hunter2-is-the-password", "AKIAEXAMPLEKEY123"}, "a hunter2-is-the-password b AKIAEXAMPLEKEY123 c hunter2-is-the"},
		"one value a prefix of the other": {[]string{"tokenabcdefgh", "tokenabcdefgh-and-the-rest"}, "pre tokenabcdefgh-and-the-rest mid tokenabcdefgh end tokenabc"},
		"a value that overlaps itself":    {[]string{"abcab"}, "x abcab y abcabcab z abca"},
	} {
		secrets := &Secrets{}
		for _, value := range tc.values {
			secrets.Add(value)
		}

		want := secrets.String(tc.text)

		scrub := func(parts ...string) string {
			var out strings.Builder

			scrubber := secrets.Writer(&out)

			for _, part := range parts {
				_, err := scrubber.Write([]byte(part))
				if err != nil {
					t.Fatal(err)
				}
			}

			err := scrubber.Flush()
			if err != nil {
				t.Fatal(err)
			}

			return out.String()
		}

		for i := 0; i <= len(tc.text); i++ {
			if got := scrub(tc.text[:i], tc.text[i:]); got != want {
				t.Fatalf("%s: split at %d: %q, want %q", name, i, got, want)
			}

			for j := i; j <= len(tc.text); j++ {
				if got := scrub(tc.text[:i], tc.text[i:j], tc.text[j:]); got != want {
					t.Fatalf("%s: splits at %d and %d: %q, want %q", name, i, j, got, want)
				}
			}
		}
	}
}

// Only a tail that could still become a value is held back, and all of it that could.
func TestScrubberHoldsExactlyAPossibleValuePrefix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, write, out, held string
	}{
		{"a prefix of the value", "ab tokena", "ab ", "tokena"},
		{"all but the last byte of the value", "ab tokenabcdefg", "ab ", "tokenabcdefg"},
		{"the whole value is masked, nothing held", "ab tokenabcdefgh", "ab ***", ""},
		{"a first byte alone", "ab t", "ab ", "t"},
		{"not a prefix", "ab tokenx", "ab tokenx", ""},
		{"nothing like the value", "ab x", "ab x", ""},
		{"shorter than the value", "t", "", "t"},
	} {
		secrets := &Secrets{}
		secrets.Add("tokenabcdefgh")

		var out strings.Builder

		scrubber := secrets.Writer(&out)

		_, err := scrubber.Write([]byte(tc.write))
		if err != nil {
			t.Fatal(err)
		}

		if out.String() != tc.out || scrubber.pending != tc.held {
			t.Errorf("%s: wrote %q, passed %q and held %q; want %q and %q", tc.name, tc.write, out.String(), scrubber.pending, tc.out, tc.held)
		}
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	return 0, w.err
}

func TestScrubberReportsAFailedWrite(t *testing.T) {
	t.Parallel()

	boom := errors.New("disk full")
	secrets := &Secrets{}
	secrets.Add("tokenabcdefgh")

	scrubber := secrets.Writer(failingWriter{boom})

	n, err := scrubber.Write([]byte("plain data"))
	if !errors.Is(err, boom) || n != 0 {
		t.Errorf("Write to a failing stream = %d, %v; want 0 and its error", n, err)
	}

	err = scrubber.Flush()
	if err != nil {
		t.Errorf("Flush with nothing held = %v, want nil", err)
	}

	held := secrets.Writer(failingWriter{boom})

	_, err = held.Write([]byte("tokena"))
	if err != nil {
		t.Fatalf("holding back writes nothing, so it cannot fail: %v", err)
	}

	err = held.Flush()
	if !errors.Is(err, boom) {
		t.Errorf("Flush to a failing stream = %v, want its error", err)
	}
}

func TestErrorMasksTheTextAndKeepsTheChain(t *testing.T) {
	t.Parallel()

	secrets := &Secrets{}
	secrets.Add("tokenabcdefgh")

	err := secrets.Error(nil)
	if err != nil {
		t.Errorf("Error(nil) = %v, want nil", err)
	}

	clean := errors.New("nothing to hide")
	got := secrets.Error(clean)
	if got != clean { //nolint:errorlint // identity is the point: an error with nothing to mask passes through untouched
		t.Errorf("an error with nothing to mask = %v, want the same error", got)
	}

	sentinel := errors.New("sentinel")
	leaky := fmt.Errorf("dial with tokenabcdefgh failed: %w", sentinel)
	masked := secrets.Error(leaky)

	if strings.Contains(masked.Error(), "tokenabcdefgh") || masked.Error() != "dial with *** failed: sentinel" {
		t.Errorf("masked = %q, want the value hidden", masked.Error())
	}

	if !errors.Is(masked, sentinel) {
		t.Error("the masked error lost its chain")
	}
}
