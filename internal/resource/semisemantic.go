package resource

// The version order Concourse's github-release resource sorts by: a port of github.com/cppforlife/go-semi-semantic (MIT, Copyright (c) 2016 Dmitriy Kalinin), the parse and Compare only. Ported rather than imported because that module's own tests pull ginkgo and gomega into go.mod.

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	semiVersionPattern = regexp.MustCompile(`\A(?P<release>[0-9A-Za-z_\.]+)(\-(?P<pre_release>[0-9A-Za-z_\-\.]+))?(\+(?P<post_release>[0-9A-Za-z_\-\.]+))?\z`) //nolint:gochecknoglobals // compiled once, read-only
	semiIntPattern     = regexp.MustCompile(`\A[0-9]+\z`)                                                                                                      //nolint:gochecknoglobals // compiled once, read-only
	semiZeroLedPattern = regexp.MustCompile(`\A0+[0-9]+\z`)                                                                                                    //nolint:gochecknoglobals // compiled once, read-only
	semiStrPattern     = regexp.MustCompile(`\A[0-9A-Za-z_\-]+\z`)                                                                                             //nolint:gochecknoglobals // compiled once, read-only
)

var errNoVersion = errors.New("not a version")

// semiComponent is one dot-separated piece: a number, or a word when it is not one (a zero-led number such as 01 is a word, as upstream reads it).
type semiComponent struct {
	isInt bool
	n     int
	s     string
}

// semiSegment is a release, pre-release or post-release part; empty when the version has none.
type semiSegment []semiComponent

type semiVersion struct {
	release, pre, post semiSegment
}

func parseSemiVersion(raw string) (semiVersion, error) {
	if raw == "" {
		return semiVersion{}, errNoVersion
	}

	match := semiVersionPattern.FindStringSubmatch(raw)
	if match == nil {
		return semiVersion{}, fmt.Errorf("%w: %q", errNoVersion, raw)
	}

	var parts [3]semiSegment

	for i, name := range []string{"release", "pre_release", "post_release"} {
		piece := match[semiVersionPattern.SubexpIndex(name)]
		if piece == "" {
			continue
		}

		segment, err := parseSemiSegment(piece)
		if err != nil {
			return semiVersion{}, err
		}

		parts[i] = segment
	}

	return semiVersion{release: parts[0], pre: parts[1], post: parts[2]}, nil
}

func parseSemiSegment(raw string) (semiSegment, error) {
	pieces := strings.Split(raw, ".")
	segment := make(semiSegment, 0, len(pieces))

	for _, piece := range pieces {
		switch {
		case semiIntPattern.MatchString(piece) && !semiZeroLedPattern.MatchString(piece):
			n, err := strconv.Atoi(piece)
			if err != nil {
				return nil, fmt.Errorf("%w: %q: %w", errNoVersion, raw, err)
			}

			segment = append(segment, semiComponent{isInt: true, n: n})
		case semiStrPattern.MatchString(piece):
			segment = append(segment, semiComponent{s: piece})
		default:
			return nil, fmt.Errorf("%w: %q has an empty or unreadable part", errNoVersion, raw)
		}
	}

	return segment, nil
}

// Compare is upstream's: release first; a version with no pre-release sorts after one with; one with a post-release after one without.
func (v semiVersion) Compare(other semiVersion) int {
	if order := v.release.compare(other.release); order != 0 {
		return order
	}

	if order := compareOptional(v.pre, other.pre, 1); order != 0 {
		return order
	}

	return compareOptional(v.post, other.post, -1)
}

// compareOptional compares a part only one of the two may have; absent is what the missing side compares as, before the two are walked.
func compareOptional(a, b semiSegment, absent int) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return absent
	case len(b) == 0:
		return -absent
	}

	return a.compare(b)
}

// compare walks the shared length, then a longer segment is greater unless what it adds is all zeros: 1.0 and 1.0.0 are equal.
func (s semiSegment) compare(other semiSegment) int {
	shared := min(len(s), len(other))

	for i := range shared {
		if order := s[i].compare(other[i]); order != 0 {
			return order
		}
	}

	switch {
	case len(s) > shared && !allZero(s[shared:]):
		return 1
	case len(other) > shared && !allZero(other[shared:]):
		return -1
	}

	return 0
}

// compare puts a word after any number, as upstream does; two words compare as bytes.
func (c semiComponent) compare(other semiComponent) int {
	switch {
	case c.isInt && other.isInt:
		return cmpInt(c.n, other.n)
	case !c.isInt && other.isInt:
		return 1
	case c.isInt && !other.isInt:
		return -1
	}

	return strings.Compare(c.s, other.s)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}

func allZero(components semiSegment) bool {
	for _, c := range components {
		if !c.isInt || c.n != 0 {
			return false
		}
	}

	return true
}
