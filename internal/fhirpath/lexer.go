package fhirpath

import (
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Error is a lexing or parsing failure, carrying the offset so callers can
// point at the offending character. The conformance suite distinguishes syntax
// errors from semantic and execution ones, so keeping this type distinct
// matters for reporting.
type Error struct {
	Msg string
	Pos int
	Src string
}

func (e *Error) Error() string {
	return fmt.Sprintf("fhirpath: %s at offset %d in %q", e.Msg, e.Pos, e.Src)
}

type lexer struct {
	src  string
	pos  int
	toks []Token
}

// lex tokenizes a FHIRPath expression.
func lex(src string) ([]Token, error) {
	l := &lexer{src: src}
	if err := l.run(); err != nil {
		return nil, err
	}
	return l.toks, nil
}

func (l *lexer) errorf(pos int, format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...), Pos: pos, Src: l.src}
}

func (l *lexer) run() error {
	for {
		l.skipSpaceAndComments()
		if l.pos >= len(l.src) {
			l.emit(Token{Kind: EOF, Pos: l.pos})
			return nil
		}
		start := l.pos
		c := l.src[l.pos]

		switch {
		case c == '\'':
			text, err := l.quoted('\'')
			if err != nil {
				return err
			}
			l.emit(Token{Kind: String, Text: text, Pos: start})

		case c == '"':
			// Not legal FHIRPath: the grammar delimits strings with single
			// quotes only, and the official test suite never uses double quotes
			// as delimiters. Accepted anyway, because published specification
			// content does -- the R5 invariant ElementDefinition eld-11 is
			// written type.code.contains(":"). It is the sole such expression in
			// either release's 4636 published expressions, and every reference
			// implementation tolerates it, which is why the slip survived.
			//
			// Rejecting it would mean refusing to evaluate a real invariant and
			// silently weakening StructureDefinition validation. Accepting costs
			// nothing: no conformance test expects a double quote to be an
			// error. See TestDoubleQuotedStringIsAcceptedForEld11.
			text, err := l.quoted('"')
			if err != nil {
				return err
			}
			l.emit(Token{Kind: String, Text: text, Pos: start})

		case c == '`':
			text, err := l.quoted('`')
			if err != nil {
				return err
			}
			l.emit(Token{Kind: DelimitedIdent, Text: text, Pos: start})

		case c == '@':
			l.pos++
			for l.pos < len(l.src) && isDateTimeByte(l.src[l.pos]) {
				l.pos++
			}
			if l.pos == start+1 {
				return l.errorf(start, "'@' must be followed by a date or time literal")
			}
			l.emit(Token{Kind: DateTime, Text: l.src[start+1 : l.pos], Pos: start})

		case c >= '0' && c <= '9':
			l.number(start)

		case c == '$':
			l.pos++
			nameStart := l.pos
			for l.pos < len(l.src) && isIdentByte(l.src[l.pos]) {
				l.pos++
			}
			if l.pos == nameStart {
				return l.errorf(start, "'$' must be followed by a name")
			}
			l.emit(Token{Kind: Dollar, Text: l.src[nameStart:l.pos], Pos: start})

		case isIdentStart(c):
			for l.pos < len(l.src) && isIdentByte(l.src[l.pos]) {
				l.pos++
			}
			l.emit(Token{Kind: Ident, Text: l.src[start:l.pos], Pos: start})

		case c == '{':
			// "{}" is the empty-collection literal; a lone "{" is not FHIRPath.
			l.pos++
			l.skipSpaceAndComments()
			if l.pos >= len(l.src) || l.src[l.pos] != '}' {
				return l.errorf(start, "'{' must be closed immediately as the empty collection '{}'")
			}
			l.pos++
			l.emit(Token{Kind: EmptyCollection, Pos: start})

		default:
			if err := l.operator(start); err != nil {
				return err
			}
		}
	}
}

// operator lexes punctuation and symbolic operators, longest match first so
// "<=" never lexes as "<" then "=".
func (l *lexer) operator(start int) error {
	two := ""
	if l.pos+1 < len(l.src) {
		two = l.src[l.pos : l.pos+2]
	}
	switch two {
	case "!=":
		l.pos += 2
		l.emit(Token{Kind: Neq, Pos: start})
		return nil
	case "!~":
		l.pos += 2
		l.emit(Token{Kind: NotEquiv, Pos: start})
		return nil
	case "<=":
		l.pos += 2
		l.emit(Token{Kind: Lte, Pos: start})
		return nil
	case ">=":
		l.pos += 2
		l.emit(Token{Kind: Gte, Pos: start})
		return nil
	}

	kinds := map[byte]Kind{
		'.': Dot, ',': Comma, '(': LParen, ')': RParen, '[': LBracket, ']': RBracket,
		'+': Plus, '-': Minus, '*': Star, '/': Slash, '&': Amp, '|': Pipe,
		'=': Eq, '~': Equiv, '<': Lt, '>': Gt, '%': Percent,
	}
	c := l.src[l.pos]
	kind, ok := kinds[c]
	if !ok {
		r, _ := utf8.DecodeRuneInString(l.src[l.pos:])
		return l.errorf(start, "unexpected character %q", r)
	}
	l.pos++
	l.emit(Token{Kind: kind, Pos: start})
	return nil
}

// number lexes an integer, decimal, or long. The fractional part is consumed
// only when a digit follows the dot, so "1.is(Integer)" lexes as 1, '.', is —
// not as the malformed number "1.".
func (l *lexer) number(start int) {
	for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
		l.pos++
	}
	if l.pos < len(l.src) && l.src[l.pos] == '.' &&
		l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1]) {
		l.pos++
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.pos++
		}
		l.emit(Token{Kind: Number, Text: l.src[start:l.pos], Pos: start})
		return
	}
	// An "L" suffix marks a long, but only when it does not run into a longer
	// word: "3L" is a long, "3Later" is not.
	if l.pos < len(l.src) && l.src[l.pos] == 'L' &&
		(l.pos+1 >= len(l.src) || !isIdentByte(l.src[l.pos+1])) {
		text := l.src[start:l.pos]
		l.pos++
		l.emit(Token{Kind: LongNumber, Text: text, Pos: start})
		return
	}
	l.emit(Token{Kind: Number, Text: l.src[start:l.pos], Pos: start})
}

// quoted lexes a string or delimited identifier, decoding escapes.
func (l *lexer) quoted(delim byte) (string, error) {
	start := l.pos
	l.pos++ // opening delimiter
	var b strings.Builder
	for {
		if l.pos >= len(l.src) {
			return "", l.errorf(start, "unterminated %s", quotedKindName(delim))
		}
		c := l.src[l.pos]
		if c == delim {
			l.pos++
			return b.String(), nil
		}
		if c != '\\' {
			b.WriteByte(c)
			l.pos++
			continue
		}
		l.pos++
		if l.pos >= len(l.src) {
			return "", l.errorf(start, "unterminated escape sequence")
		}
		esc := l.src[l.pos]
		l.pos++
		switch esc {
		case '\'', '"', '`', '\\', '/':
			b.WriteByte(esc)
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, err := l.unicodeEscape()
			if err != nil {
				return "", err
			}
			b.WriteRune(r)
		default:
			return "", l.errorf(l.pos-2, "unknown escape sequence %q", "\\"+string(esc))
		}
	}
}

// unicodeEscape decodes \uXXXX, combining a surrogate pair when one follows so
// astral characters survive round-tripping.
func (l *lexer) unicodeEscape() (rune, error) {
	hi, err := l.hex4()
	if err != nil {
		return 0, err
	}
	if !utf16.IsSurrogate(rune(hi)) {
		return rune(hi), nil
	}
	// A high surrogate is only meaningful paired with a low one.
	if l.pos+1 < len(l.src) && l.src[l.pos] == '\\' && l.src[l.pos+1] == 'u' {
		save := l.pos
		l.pos += 2
		lo, err := l.hex4()
		if err != nil {
			return 0, err
		}
		if r := utf16.DecodeRune(rune(hi), rune(lo)); r != utf8.RuneError {
			return r, nil
		}
		l.pos = save
	}
	return utf8.RuneError, nil
}

func (l *lexer) hex4() (int, error) {
	if l.pos+4 > len(l.src) {
		return 0, l.errorf(l.pos, "\\u needs four hex digits")
	}
	v := 0
	for i := 0; i < 4; i++ {
		c := l.src[l.pos+i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, l.errorf(l.pos+i, "invalid hex digit %q in \\u escape", rune(c))
		}
		v = v*16 + d
	}
	l.pos += 4
	return v, nil
}

// skipSpaceAndComments consumes whitespace, // line comments, and /* */ block
// comments. Comments are checked before the division operator, so "a // b" is a
// comment rather than two divisions.
func (l *lexer) skipSpaceAndComments() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			l.pos++
		case c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*':
			l.pos += 2
			for l.pos+1 < len(l.src) && !(l.src[l.pos] == '*' && l.src[l.pos+1] == '/') {
				l.pos++
			}
			// An unterminated block comment simply runs to end of input; the
			// parser reports the resulting truncation.
			if l.pos+1 < len(l.src) {
				l.pos += 2
			} else {
				l.pos = len(l.src)
			}
		default:
			return
		}
	}
}

func (l *lexer) emit(t Token) { l.toks = append(l.toks, t) }

func quotedKindName(delim byte) string {
	if delim == '`' {
		return "delimited identifier"
	}
	return "string literal"
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentByte(c byte) bool { return isIdentStart(c) || isDigit(c) }

// isDateTimeByte reports whether c can appear in an @-literal: dates, times,
// fractional seconds, and timezone offsets.
func isDateTimeByte(c byte) bool {
	return isDigit(c) || c == '-' || c == ':' || c == '.' ||
		c == 'T' || c == 'Z' || c == '+'
}
