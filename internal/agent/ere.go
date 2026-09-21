package agent

// Rendering a model's Go regexp as the POSIX ERE a container's grep is given.

import (
	"errors"
	"fmt"
	"regexp/syntax"
	"strings"
	"unicode"
)

// asciiMax is the last rune a POSIX bracket expression can name portably. Above it a container's grep is byte-oriented under the C locale and a multi-byte range stops meaning what RE2 meant by it.
const asciiMax = 0x7F

// unicodeMax is the top of RE2's rune space, and the marker that a negated class means "everything else" rather than a named alphabet.
const unicodeMax = 0x10FFFF

// errUnportablePattern is a pattern RE2 accepts and POSIX ERE cannot express. It is returned to the model as tool data rather than failing the step: the model wrote the pattern and is the one who can rewrite it.
var errUnportablePattern = errors.New("search_files: pattern")

// patternToERE renders pattern — already valid RE2, since search_files compiled it — as a POSIX ERE. Going through the AST rather than substituting strings is what makes it exact: RE2's own parser has already turned `\d`, `\w`, `\s` and `\p{...}` into explicit character classes, so there is no translation table to get subtly wrong. Passing the pattern through instead was measured to be unsafe — alpine's busybox grep reads `\d` as a digit class and debian's GNU grep reads it as the letter d, so a pipeline's search results would depend on its image, which is the one property placement must not change.
func patternToERE(pattern string) (string, error) {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", fmt.Errorf("%w %q is not a valid regular expression: %w", errUnportablePattern, pattern, err)
	}

	var out strings.Builder

	err = renderERE(parsed, &out, false)
	if err != nil {
		return "", fmt.Errorf("%w %q: %w", errUnportablePattern, pattern, err)
	}

	return out.String(), nil
}

// renderERE writes re as ERE into out. wrap asks for a group around anything whose top level is an alternation or a concatenation, which is what a quantifier needs applied to it.
func renderERE(re *syntax.Regexp, out *strings.Builder, wrap bool) error {
	if re.Flags&syntax.NonGreedy != 0 {
		return errNonGreedy
	}

	if literal, ok := ereAtom(re.Op); ok {
		out.WriteString(literal)

		return nil
	}

	if isQuantifier(re.Op) {
		return renderRepeat(re, out)
	}

	switch re.Op { //nolint:exhaustive // the fixed-spelling ops go through ereAtom and the quantifiers through isQuantifier, both above; default refuses anything RE2 grows that POSIX cannot say
	case syntax.OpEmptyMatch, syntax.OpNoMatch:
		return nil
	case syntax.OpLiteral:
		return renderLiteral(re, out, wrap)
	case syntax.OpCharClass:
		return renderCharClass(re, out)
	case syntax.OpCapture:
		return renderGroup(re.Sub[0], out)
	case syntax.OpConcat, syntax.OpAlternate:
		return renderBranch(re, out, wrap)
	default:
		return fmt.Errorf("%w: %v", errUnsupportedOp, re.Op)
	}
}

// isQuantifier is the four ops that repeat whatever they hold.
func isQuantifier(op syntax.Op) bool {
	return op == syntax.OpStar || op == syntax.OpPlus || op == syntax.OpQuest || op == syntax.OpRepeat
}

// ereAtom is every RE2 op that renders as a fixed piece of ERE, which is also every op whose meaning grep is line-oriented enough to preserve: a text anchor and a line anchor are the same thing when the subject is one line.
func ereAtom(op syntax.Op) (string, bool) {
	switch op { //nolint:exhaustive // every op with operands is rendered by renderERE, which is the caller
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return ".", true
	case syntax.OpBeginLine, syntax.OpBeginText:
		return "^", true
	case syntax.OpEndLine, syntax.OpEndText:
		return "$", true
	case syntax.OpWordBoundary:
		return `\b`, true
	case syntax.OpNoWordBoundary:
		return `\B`, true
	default:
		return "", false
	}
}

var (
	// errNonGreedy is the one RE2 construct with no ERE spelling that a model reaches for by habit. Measured on both alpine and debian: `a.*?X` under -E matches greedily and reports it as success, so passing it through would answer the wrong question silently.
	errNonGreedy = errors.New("a non-greedy quantifier (*?, +?, ??) has no POSIX form and silently matches greedily; rewrite it, e.g. a[^X]*X instead of a.*?X")
	// errUnicodeClass names the case a bracket expression cannot carry: a class that holds some runes above ASCII but not all of them, which is what \p{Greek} and friends are.
	errUnicodeClass = errors.New("a Unicode character class (\\p{...}) has no POSIX form; name the characters, or use an ASCII class")
	// errMultilinePattern is a pattern that can only match across a line break, which a line-oriented grep can never do.
	errMultilinePattern = errors.New("the pattern contains a newline, and search is line-oriented")
	errUnsupportedOp    = errors.New("unsupported expression")
)

// renderLiteral writes a run of literal runes, expanding a case-folded one into the bracket expression that spells both cases. A fold survives RE2's parse as a flag on the literal, so this is the only place `(?i)` has to be understood.
func renderLiteral(re *syntax.Regexp, out *strings.Builder, wrap bool) error {
	folded := re.Flags&syntax.FoldCase != 0
	if wrap && (len(re.Rune) > 1) {
		out.WriteByte('(')
		defer out.WriteByte(')')
	}

	for _, r := range re.Rune {
		if r == '\n' {
			return errMultilinePattern
		}

		if !folded {
			writeLiteralRune(r, out)

			continue
		}

		cases := caseFolds(r)
		if len(cases) == 1 {
			writeLiteralRune(r, out)

			continue
		}

		out.WriteByte('[')

		// Every rune here is a case variant of a letter, so none of them is one of classSpecials and none needs a position.
		for _, c := range cases {
			out.WriteRune(c)
		}

		out.WriteByte(']')
	}

	return nil
}

// caseFolds is every case variant of r, in ascending order. unicode.SimpleFold walks a cycle, so following it until it comes back around collects the whole set.
func caseFolds(r rune) []rune {
	out := []rune{r}

	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		out = append(out, f)
	}

	if len(out) > 1 && out[1] < out[0] {
		out[0], out[1] = out[1], out[0]
	}

	return out
}

// renderCharClass writes a class as a bracket expression, positive when it names only ASCII and negated when it covers everything above ASCII — which is how RE2 records `[^abc]` and `.`-like complements. A class holding SOME high runes is a named alphabet, and is refused rather than silently narrowed to its ASCII part.
func renderCharClass(re *syntax.Regexp, out *strings.Builder) error {
	pairs := re.Rune
	if len(pairs) == 0 {
		return errUnsupportedOp
	}

	coversHigh := pairs[len(pairs)-2] <= asciiMax+1 && pairs[len(pairs)-1] == unicodeMax

	highRanges := 0

	for i := 0; i < len(pairs); i += 2 {
		if pairs[i+1] > asciiMax {
			highRanges++
		}
	}

	switch {
	case highRanges == 0:
		return writeClass(pairs, out, false)
	case highRanges == 1 && coversHigh:
		return writeClass(complementASCII(pairs), out, true)
	default:
		return errUnicodeClass
	}
}

// complementASCII is every ASCII rune the class does NOT hold, as range pairs — what a negated bracket expression has to name.
func complementASCII(pairs []rune) []rune {
	var (
		out  []rune
		next rune
	)

	for i := 0; i < len(pairs); i += 2 {
		lo, hi := pairs[i], pairs[i+1]
		if lo > next {
			out = append(out, next, min(lo-1, asciiMax))
		}

		if hi+1 > next {
			next = hi + 1
		}

		if next > asciiMax {
			break
		}
	}

	if next <= asciiMax {
		out = append(out, next, asciiMax)
	}

	return out
}

// classSpecials are the four characters a bracket expression can hold only by POSITION, because a backslash inside one is an ordinary character rather than an escape: `]` goes first, `^` anywhere but first, `-` last, and `\` anywhere.
//
// The collating-symbol spelling (`[.-.]`) that GNU grep accepts is NOT an option, and that was measured rather than assumed: musl's regcomp — what busybox grep links against on alpine — answers `[[.-.]0-9A-Z_a-z]` with "Unknown collating element" and exits 2, which is indistinguishable from finding nothing. A pattern as ordinary as `[a-zA-Z0-9_-]+` would then return an empty search on one image and every match on another, which is the one property placement must not change.
const classSpecials = `]^-\`

// writeClass writes range pairs as a bracket expression, with the newline taken out of every range it falls inside. Dropping it costs nothing — grep never sees a newline within a line, so no class can be asked about one — and keeping it is fatal: the rendered pattern is one argument, and a raw newline in it is how grep is told it has been given a SECOND pattern to match as an alternative. `\s` is the everyday case, since RE2 expands it to a range spanning tab through carriage return.
func writeClass(pairs []rune, out *strings.Builder, negated bool) error {
	ranges, specials := liftClassSpecials(pairs)

	body := classBody(ranges, specials, negated)
	if body == "" {
		return errUnsupportedOp
	}

	out.WriteByte('[')

	if negated {
		out.WriteByte('^')
	}

	out.WriteString(body)
	out.WriteByte(']')

	return nil
}

// classBody spells the members of a bracket expression, in the one order that reads them all as literals.
func classBody(ranges [][2]rune, specials map[rune]bool, negated bool) string {
	var body strings.Builder

	if specials[']'] {
		body.WriteByte(']')
	}

	for _, r := range ranges {
		body.WriteRune(r[0])

		if r[1] != r[0] {
			body.WriteByte('-')
			body.WriteRune(r[1])
		}
	}

	if specials['\\'] {
		body.WriteByte('\\')
	}

	if specials['^'] {
		// A `^` opening a bracket negates it. Only `-` can stand in front of it without needing a position of its own, and one of the two is always available: RE2 renders a one-character class as a literal, never as a class, so `^` is never alone in here.
		if body.Len() == 0 && !negated && specials['-'] {
			body.WriteByte('-')
			delete(specials, '-')
		}

		body.WriteByte('^')
	}

	if specials['-'] {
		body.WriteByte('-')
	}

	return body.String()
}

// liftClassSpecials takes the newline out of every range and pulls each classSpecials character off a range's ENDS, where its own spelling would be read as syntax. One strictly INSIDE a range needs no lifting: the range names it without ever writing it.
func liftClassSpecials(pairs []rune) ([][2]rune, map[rune]bool) {
	specials := map[rune]bool{}

	var ranges [][2]rune

	for i := 0; i < len(pairs); i += 2 {
		for _, r := range splitAroundNewline(pairs[i], pairs[i+1]) {
			lo, hi := r[0], r[1]

			for lo <= hi && strings.ContainsRune(classSpecials, lo) {
				specials[lo] = true
				lo++
			}

			for lo <= hi && strings.ContainsRune(classSpecials, hi) {
				specials[hi] = true
				hi--
			}

			if lo <= hi {
				ranges = append(ranges, [2]rune{lo, hi})
			}
		}
	}

	return ranges, specials
}

// splitAroundNewline is lo..hi with '\n' removed, as up to two ranges.
func splitAroundNewline(lo, hi rune) [][2]rune {
	if lo > '\n' || hi < '\n' {
		return [][2]rune{{lo, hi}}
	}

	var out [][2]rune

	if lo < '\n' {
		out = append(out, [2]rune{lo, '\n' - 1})
	}

	if hi > '\n' {
		out = append(out, [2]rune{'\n' + 1, hi})
	}

	return out
}

// renderRepeat writes a quantifier and the expression it applies to, grouping that expression whenever it is more than one thing.
func renderRepeat(re *syntax.Regexp, out *strings.Builder) error {
	err := renderERE(re.Sub[0], out, true)
	if err != nil {
		return err
	}

	switch re.Op { //nolint:exhaustive // renderERE dispatches only the four quantifiers here
	case syntax.OpStar:
		out.WriteByte('*')
	case syntax.OpPlus:
		out.WriteByte('+')
	case syntax.OpQuest:
		out.WriteByte('?')
	case syntax.OpRepeat:
		writeInterval(re, out)
	default:
		return fmt.Errorf("%w: %v", errUnsupportedOp, re.Op)
	}

	return nil
}

// writeInterval writes {n}, {n,} or {n,m}, the three spellings ERE shares with RE2.
func writeInterval(re *syntax.Regexp, out *strings.Builder) {
	switch {
	case re.Max == re.Min:
		fmt.Fprintf(out, "{%d}", re.Min)
	case re.Max < 0:
		fmt.Fprintf(out, "{%d,}", re.Min)
	default:
		fmt.Fprintf(out, "{%d,%d}", re.Min, re.Max)
	}
}

func renderGroup(sub *syntax.Regexp, out *strings.Builder) error {
	out.WriteByte('(')

	err := renderERE(sub, out, false)
	if err != nil {
		return err
	}

	out.WriteByte(')')

	return nil
}

// renderBranch writes a concatenation or an alternation, which differ only in whether a separator goes between the parts and in what each part has to be wrapped against.
func renderBranch(re *syntax.Regexp, out *strings.Builder, wrap bool) error {
	if wrap {
		return renderGroup(re, out)
	}

	if re.Op == syntax.OpAlternate {
		return renderAlternate(re, out)
	}

	return renderConcat(re, out)
}

func renderConcat(re *syntax.Regexp, out *strings.Builder) error {
	for _, sub := range re.Sub {
		err := renderERE(sub, out, sub.Op == syntax.OpAlternate)
		if err != nil {
			return err
		}
	}

	return nil
}

func renderAlternate(re *syntax.Regexp, out *strings.Builder) error {
	for i, sub := range re.Sub {
		if i > 0 {
			out.WriteByte('|')
		}

		err := renderERE(sub, out, false)
		if err != nil {
			return err
		}
	}

	return nil
}

// ereSpecial are the characters a bracket expression cannot hold literally, plus the ones that would otherwise be operators outside one.
const ereSpecial = `.^$*+?()[]{}|\`

// writeLiteralRune writes one rune as itself, escaping what ERE would otherwise read as syntax. A rune outside printable ASCII is written as the bytes of its UTF-8 encoding, which is what a byte-oriented grep compares against.
func writeLiteralRune(r rune, out *strings.Builder) {
	if r <= asciiMax && strings.ContainsRune(ereSpecial, r) {
		out.WriteByte('\\')
	}

	out.WriteRune(r)
}
