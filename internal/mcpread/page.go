package mcpread

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jtarchie/steps/internal/runview"
)

const (
	// responseCap is the most one tool call answers with, structured and text together: an agent reads every byte of it into its context, and a run's whole log in one answer is what pocketci did.
	responseCap = 64 << 10
	// dataBudget is what the structured half may take. The text half repeats it under a one-line frame, so twice this plus the frame stays inside responseCap.
	dataBudget = 30 << 10
	// defaultTextLimit and maxTextLimit bound one page of output or configuration, in bytes of text before JSON escaping; a page that escapes past dataBudget is halved until it fits.
	defaultTextLimit = 8 << 10
	maxTextLimit     = 24 << 10
	defaultListLimit = 50
	maxListLimit     = 200
	// fieldCap bounds one free-text field — an error, a message, a name — so a single list item can never be bigger than a page.
	fieldCap = 2 << 10
)

var errBadCursor = errors.New("that cursor is not one this server gave out; start again without it")

// encodeCursor makes a position opaque, so nobody builds one by hand and expects it to keep meaning the same thing.
func encodeCursor(kind, value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + value))
}

func decodeCursor(cursor, kind string) (string, error) {
	if cursor == "" {
		return "", nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", errBadCursor
	}

	value, found := strings.CutPrefix(string(raw), kind+":")
	if !found {
		return "", errBadCursor
	}

	return value, nil
}

func offsetCursor(cursor string) (int, error) {
	value, err := decodeCursor(cursor, "o")
	if err != nil || value == "" {
		return 0, err
	}

	offset, err := strconv.Atoi(value)
	if err != nil || offset < 0 {
		return 0, errBadCursor
	}

	return offset, nil
}

func listLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}

	return min(limit, maxListLimit)
}

// fits is whether v encodes within the structured half's budget.
func fits(v any) bool {
	encoded, err := json.Marshal(v)

	return err == nil && len(encoded) <= dataBudget
}

// window is items[start:end] for the page a cursor starts, shrunk until wrap of it fits, and the cursor of the page after it.
func window[T, O any](items []T, cursor string, limit int, wrap func(page []T, next string) O) (O, error) {
	start, err := offsetCursor(cursor)
	if err != nil {
		var zero O

		return zero, err
	}

	if len(items) == 0 {
		return wrap([]T{}, ""), nil
	}

	start = min(start, len(items))
	end := min(start+listLimit(limit), len(items))

	for {
		next := ""
		if end < len(items) {
			next = encodeCursor("o", strconv.Itoa(end))
		}

		out := wrap(items[start:end], next)
		if fits(out) || end-start <= 1 {
			return out, nil
		}

		end = start + (end-start)/2
	}
}

// textPage is text[offset:offset+limit] cut on rune boundaries, shrunk until wrap of it fits, and where the next page starts (0 when this one reaches the end).
func textPage[O any](text string, offset, limit int, wrap func(chunk string, next int) O) O {
	if limit <= 0 {
		limit = defaultTextLimit
	}

	limit = min(limit, maxTextLimit)
	offset = runeStart(text, min(max(offset, 0), len(text)))

	for {
		end := runeStart(text, min(offset+limit, len(text)))
		if end == offset && offset < len(text) {
			_, size := utf8.DecodeRuneInString(text[offset:])
			end = offset + size
		}

		next := 0
		if end < len(text) {
			next = end
		}

		out := wrap(text[offset:end], next)
		if fits(out) || limit <= 1 {
			return out
		}

		limit /= 2
	}
}

// runeStart moves i back to the start of the rune it lands in, so a page never splits one.
func runeStart(text string, i int) int {
	for i > 0 && i < len(text) && !utf8.RuneStart(text[i]) {
		i--
	}

	return i
}

// clean is free text made safe to hand an agent: escape sequences and control characters gone (runview.Scrub, which the terminal uses for the same reason), and at most fieldCap bytes.
func clean(text string) string {
	text = runview.Scrub(text)
	if len(text) <= fieldCap {
		return text
	}

	cut := runeStart(text, fieldCap)

	return text[:cut] + "… [" + strconv.Itoa(len(text)-cut) + " more bytes]"
}

func cleanAll(texts []string) []string {
	out := make([]string, len(texts))
	for i, text := range texts {
		out[i] = clean(text)
	}

	return out
}
