package events

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Fact is one name/value pair a resource handed back about what a get fetched or a put published — Concourse's resource metadata.
type Fact struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// NoteFact names the note a get says one of its facts with, NotePublished a put's: Detail is the fact's name, Text its value.
const (
	NoteFact      = "fact"
	NotePublished = "published"
)

// The bounds on what one resource may hand back. A fact is resource output, so it is cut where it enters rather than trusted downstream.
const (
	maxFacts         = 64
	maxFactNameBytes = 128
	maxFactBytes     = 4096
)

// CleanFacts is facts as steps will show and keep them: control characters gone (a terminal escape in a value would drive the reader's terminal; a newline would forge a line), each name and value cut to its bound, nameless entries dropped, and at most maxFacts kept.
func CleanFacts(facts []Fact) []Fact {
	clean := make([]Fact, 0, min(len(facts), maxFacts))

	for _, fact := range facts {
		name := cleanFactText(fact.Name, maxFactNameBytes)
		if name == "" {
			continue
		}

		clean = append(clean, Fact{Name: name, Value: cleanFactText(fact.Value, maxFactBytes)})
		if len(clean) == maxFacts {
			break
		}
	}

	return clean
}

func cleanFactText(text string, limit int) string {
	text = strings.ToValidUTF8(text, "�")
	text = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r):
			return -1
		}

		return r
	}, text)
	text = strings.TrimSpace(text)

	if len(text) <= limit {
		return text
	}

	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}

	return text[:cut]
}

// FactNote says one fact on the step ctx names; published is a put's, which a reader lists among what the run published.
func FactNote(ctx context.Context, fact Fact, published bool) {
	name := NoteFact
	if published {
		name = NotePublished
	}

	note(ctx, Event{Status: NoteInfo, Text: fact.Value, Name: name, Detail: fact.Name})
}

// IsFactNote reports a note that carries a fact rather than a line.
func IsFactNote(event Event) bool {
	return event.Type == TypeStepNote && (event.Name == NoteFact || event.Name == NotePublished)
}

// FactLink is the fact's value as a link a reader may follow: only an absolute http or https URL, never javascript:, data: or a relative path.
func FactLink(value string) (string, bool) {
	if strings.ContainsAny(value, " \"'<>`") {
		return "", false
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return "", false
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return parsed.String(), true
	}

	return "", false
}

// EncodeFacts is facts as the state database keeps them beside a version; none is "".
func EncodeFacts(facts []Fact) string {
	if len(facts) == 0 {
		return ""
	}

	encoded, err := json.Marshal(facts)
	if err != nil {
		return ""
	}

	return string(encoded)
}

// DecodeFacts reads what EncodeFacts kept; anything unreadable is none, since a fact is shown, never acted on.
func DecodeFacts(encoded string) []Fact {
	if encoded == "" {
		return nil
	}

	var facts []Fact

	err := json.Unmarshal([]byte(encoded), &facts)
	if err != nil {
		return nil
	}

	return CleanFacts(facts)
}
