package redact

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
)

const mask = "***"

// minLine is the shortest line of a multi-line value masked on its own.
const minLine = 8

// Secrets is one run's resolved values, growing as the run resolves more; a nil *Secrets scrubs nothing.
type Secrets struct {
	mu     sync.RWMutex
	values []string
}

// Add keeps each line of a multi-line value as its own entry because a PEM key is usually printed a line at a time.
func (s *Secrets) Add(value string) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	candidates := []string{value}

	// A line shorter than minLine is structure, not secret: a JSON key file's lone { would otherwise mask every brace the run prints.
	for line := range strings.SplitSeq(value, "\n") {
		if line = strings.TrimSpace(line); len(line) >= minLine {
			candidates = append(candidates, line)
		}
	}

	for _, candidate := range candidates {
		if candidate != "" && !slices.Contains(s.values, candidate) {
			s.values = append(s.values, candidate)
		}
	}

	// Longest first, so a whole value is masked before one of its own lines could leave the rest of it showing.
	slices.SortFunc(s.values, func(a, b string) int { return len(b) - len(a) })
}

// Empty is whether there is nothing to mask, so a caller can skip copying through a scrubber.
func (s *Secrets) Empty() bool {
	if s == nil {
		return true
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.values) == 0
}

// String masks every registered value in text.
func (s *Secrets) String(text string) string {
	if s == nil {
		return text
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, value := range s.values {
		text = strings.ReplaceAll(text, value, mask)
	}

	return text
}

// heldTail is how much of text's end could still become a value once more bytes arrive; holding back only that keeps a quiet step's last line in the live view.
func (s *Secrets) heldTail(text string) int {
	if s == nil {
		return 0
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	held := 0

	for _, value := range s.values {
		// Only a position holding the value's first byte can start a held tail; the first such one in the window is the longest.
		start := max(len(text)-(len(value)-1), 0)
		for i := start; i < len(text)-held; i++ {
			offset := strings.IndexByte(text[i:len(text)-held], value[0])
			if offset < 0 {
				break
			}

			i += offset
			if strings.HasPrefix(value, text[i:]) {
				held = len(text) - i

				break
			}
		}
	}

	return held
}

// Writer scrubs a stream before it reaches w; Flush hands over the bytes it holds back.
func (s *Secrets) Writer(w io.Writer) *Scrubber {
	return &Scrubber{secrets: s, w: w}
}

// Scrubber holds back a tail that is the start of a value, so a value split across two writes is still whole when it is masked.
type Scrubber struct {
	mu      sync.Mutex
	secrets *Secrets
	w       io.Writer
	pending string
}

func (sc *Scrubber) Write(p []byte) (int, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	text := sc.secrets.String(sc.pending + string(p))

	keep := sc.secrets.heldTail(text)
	sc.pending = text[len(text)-keep:]

	_, err := io.WriteString(sc.w, text[:len(text)-keep])
	if err != nil {
		return 0, err //nolint:wrapcheck // the stream's own error
	}

	return len(p), nil
}

// Flush writes what Write held back; call it once the stream has ended.
func (sc *Scrubber) Flush() error {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	text := sc.secrets.String(sc.pending)
	sc.pending = ""

	_, err := io.WriteString(sc.w, text)

	return err //nolint:wrapcheck // the stream's own error
}

// Error masks err's text and keeps its chain, so errors.Is and errors.As still classify it; nil and an error with nothing to mask pass through.
func (s *Secrets) Error(err error) error {
	if err == nil {
		return nil
	}

	text := s.String(err.Error())
	if text == err.Error() {
		return err
	}

	return maskedError{text: text, err: err}
}

type maskedError struct {
	text string
	err  error
}

func (e maskedError) Error() string { return e.text }

func (e maskedError) Unwrap() error { return e.err }

type secretsKey struct{}

// WithSecrets carries a run's values to everything that records its output.
func WithSecrets(ctx context.Context, s *Secrets) context.Context {
	return context.WithValue(ctx, secretsKey{}, s)
}

// SecretsFrom is the run's values, nil when the run resolves none.
func SecretsFrom(ctx context.Context) *Secrets {
	s, _ := ctx.Value(secretsKey{}).(*Secrets)

	return s
}
