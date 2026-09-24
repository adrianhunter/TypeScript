// Package zig_scanner implements a lexer for the Zig programming language.
//
// Unlike the TypeScript scanner, this scanner understands Zig-specific lexical
// constructs such as `//` line comments, `\\` multiline string literals, char
// literals, arbitrary-precision numeric literals with `_` separators, builtins
// (`@import`), and quoted identifiers (`@"..."`). It is used by the Zig front
// end in internal/zig_parser to lower Zig to TypeScript.
package zig_scanner

import "strings"

// Kind classifies a scanned token.
type Kind uint8

const (
	// Invalid is produced for a byte the scanner cannot recognize.
	Invalid Kind = iota
	// EOF marks the end of the input.
	EOF
	// Identifier is a normal or quoted (`@"..."`) identifier.
	Identifier
	// Builtin is an `@`-prefixed builtin such as `@import` or `@sizeOf`.
	Builtin
	// Keyword is one of the reserved Zig keywords.
	Keyword
	// Number is an integer or floating point literal.
	Number
	// String is a double quoted or multiline string literal.
	String
	// Char is a character literal such as 'a' or '\n'.
	Char
	// Punct is an operator or punctuation token.
	Punct
)

// Token is a single lexed Zig token.
type Token struct {
	Kind  Kind
	Text  string // raw source text of the token
	Value string // decoded value (identifier name, string contents, builtin name, ...)
	Pos   int    // byte offset of the token start
	End   int    // byte offset just past the token
	Line  int    // zero-based line number of the token start
}

// IsKeyword reports whether the token is the given Zig keyword.
func (t Token) IsKeyword(keyword string) bool {
	return t.Kind == Keyword && t.Value == keyword
}

// IsPunct reports whether the token is the given punctuation/operator text.
func (t Token) IsPunct(punct string) bool {
	return t.Kind == Punct && t.Text == punct
}

// IsMultilineString reports whether the token is a `\\` multiline string literal.
func (t Token) IsMultilineString() bool {
	return t.Kind == String && strings.HasPrefix(t.Text, `\\`)
}

// Keywords is the set of reserved Zig keywords.
var Keywords = map[string]bool{
	"addrspace":      true,
	"align":          true,
	"allowzero":      true,
	"and":            true,
	"anyframe":       true,
	"anytype":        true,
	"asm":            true,
	"async":          true,
	"await":          true,
	"break":          true,
	"callconv":       true,
	"catch":          true,
	"comptime":       true,
	"const":          true,
	"continue":       true,
	"defer":          true,
	"else":           true,
	"enum":           true,
	"errdefer":       true,
	"error":          true,
	"export":         true,
	"extern":         true,
	"fn":             true,
	"for":            true,
	"if":             true,
	"inline":         true,
	"linksection":    true,
	"noalias":        true,
	"noinline":       true,
	"nosuspend":      true,
	"null":           true,
	"opaque":         true,
	"or":             true,
	"orelse":         true,
	"packed":         true,
	"pub":            true,
	"resume":         true,
	"return":         true,
	"struct":         true,
	"suspend":        true,
	"switch":         true,
	"test":           true,
	"threadlocal":    true,
	"try":            true,
	"undefined":      true,
	"union":          true,
	"unreachable":    true,
	"usingnamespace": true,
	"var":            true,
	"volatile":       true,
	"while":          true,
}

// multiCharPuncts lists the punctuation tokens longer than a single byte,
// ordered longest-first so the scanner greedily prefers the longest match.
var multiCharPuncts = []string{
	"<<=", ">>=", "**=", "...",
	"..", "=>", "->", "==", "!=", "<=", ">=", "<<", ">>", "**", "++",
	"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "||", "&&", "??", "::",
}

// Lexer turns Zig source text into a token slice.
type Lexer struct {
	text string
	pos  int
	line int
}

// NewLexer creates a lexer for the provided source text.
func NewLexer(text string) *Lexer {
	return &Lexer{text: text}
}

// Tokens lexes the entire input. Comments, whitespace and newlines are not
// included in the result.
func (l *Lexer) Tokens() []Token {
	tokens := make([]Token, 0, len(l.text)/4+1)
	for {
		token := l.scanToken()
		if token.Kind == EOF {
			tokens = append(tokens, token)
			return tokens
		}
		if token.Kind == Invalid {
			continue
		}
		tokens = append(tokens, token)
	}
}

func (l *Lexer) peek(offset int) byte {
	i := l.pos + offset
	if i < 0 || i >= len(l.text) {
		return 0
	}
	return l.text[i]
}

func (l *Lexer) skipTrivia() {
	for l.pos < len(l.text) {
		c := l.text[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			l.pos++
		case c == '\n':
			l.pos++
			l.line++
		case c == '/' && l.peek(1) == '/':
			for l.pos < len(l.text) && l.text[l.pos] != '\n' {
				l.pos++
			}
		default:
			return
		}
	}
}

func (l *Lexer) scanToken() Token {
	l.skipTrivia()
	start := l.pos
	startLine := l.line
	if l.pos >= len(l.text) {
		return Token{Kind: EOF, Pos: start, End: start, Line: startLine}
	}
	c := l.text[l.pos]
	switch {
	case c == '"':
		return l.scanString(start, startLine)
	case c == '\'':
		return l.scanChar(start, startLine)
	case c == '@':
		return l.scanAt(start, startLine)
	case c == '\\' && l.peek(1) == '\\':
		return l.scanMultilineString(start, startLine)
	case isIdentStart(c):
		return l.scanIdentifier(start, startLine)
	case c >= '0' && c <= '9':
		return l.scanNumber(start, startLine)
	default:
		return l.scanPunct(start, startLine)
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentContinue(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9'
}

func (l *Lexer) scanIdentifier(start, line int) Token {
	for l.pos < len(l.text) && isIdentContinue(l.text[l.pos]) {
		l.pos++
	}
	text := l.text[start:l.pos]
	kind := Identifier
	if Keywords[text] {
		kind = Keyword
	}
	return Token{Kind: kind, Text: text, Value: text, Pos: start, End: l.pos, Line: line}
}

func (l *Lexer) scanAt(start, line int) Token {
	l.pos++ // consume '@'
	if l.pos < len(l.text) && l.text[l.pos] == '"' {
		l.pos++ // consume opening quote
		valueStart := l.pos
		for l.pos < len(l.text) && l.text[l.pos] != '"' && l.text[l.pos] != '\n' {
			l.pos++
		}
		value := l.text[valueStart:l.pos]
		if l.pos < len(l.text) && l.text[l.pos] == '"' {
			l.pos++
		}
		return Token{Kind: Identifier, Text: l.text[start:l.pos], Value: value, Pos: start, End: l.pos, Line: line}
	}
	valueStart := l.pos
	for l.pos < len(l.text) && isIdentContinue(l.text[l.pos]) {
		l.pos++
	}
	if valueStart == l.pos {
		return Token{Kind: Invalid, Text: l.text[start:l.pos], Pos: start, End: l.pos, Line: line}
	}
	value := l.text[valueStart:l.pos]
	return Token{Kind: Builtin, Text: l.text[start:l.pos], Value: value, Pos: start, End: l.pos, Line: line}
}

func (l *Lexer) scanMultilineString(start, line int) Token {
	l.pos += 2 // consume `\\`
	valueStart := l.pos
	for l.pos < len(l.text) && l.text[l.pos] != '\n' {
		l.pos++
	}
	value := l.text[valueStart:l.pos]
	return Token{Kind: String, Text: l.text[start:l.pos], Value: value, Pos: start, End: l.pos, Line: line}
}

func (l *Lexer) scanString(start, line int) Token {
	l.pos++ // opening quote
	for l.pos < len(l.text) {
		c := l.text[l.pos]
		if c == '\\' {
			l.pos++
			if l.pos < len(l.text) {
				if l.text[l.pos] == 'x' {
					l.pos += 2
				} else {
					l.pos++
				}
			}
			continue
		}
		if c == '"' {
			l.pos++
			break
		}
		if c == '\n' {
			break
		}
		l.pos++
	}
	raw := l.text[start:l.pos]
	value := raw
	if len(raw) >= 2 && strings.HasPrefix(raw, `"`) {
		value = strings.TrimSuffix(strings.TrimPrefix(raw, `"`), `"`)
	}
	return Token{Kind: String, Text: raw, Value: value, Pos: start, End: l.pos, Line: line}
}

func (l *Lexer) scanChar(start, line int) Token {
	l.pos++ // opening quote
	for l.pos < len(l.text) {
		c := l.text[l.pos]
		if c == '\\' {
			l.pos++
			if l.pos < len(l.text) {
				if l.text[l.pos] == 'x' {
					l.pos += 2
				} else {
					l.pos++
				}
			}
			continue
		}
		if c == '\'' {
			l.pos++
			break
		}
		if c == '\n' {
			break
		}
		l.pos++
	}
	raw := l.text[start:l.pos]
	value := raw
	if len(raw) >= 2 {
		value = raw[1 : len(raw)-1]
	}
	return Token{Kind: Char, Text: raw, Value: value, Pos: start, End: l.pos, Line: line}
}

func (l *Lexer) scanNumber(start, line int) Token {
	if l.text[l.pos] == '0' && l.pos+1 < len(l.text) {
		switch l.text[l.pos+1] {
		case 'x', 'X':
			l.scanRadixNumber(16)
			return l.numberToken(start, line)
		case 'b', 'B':
			l.scanRadixNumber(2)
			return l.numberToken(start, line)
		case 'o', 'O':
			l.scanRadixNumber(8)
			return l.numberToken(start, line)
		}
	}
	l.scanDigits(10)
	if l.pos < len(l.text) && l.text[l.pos] == '.' && l.peek(1) != '.' && !isIdentStart(l.peek(1)) {
		l.pos++
		l.scanDigits(10)
	}
	if l.pos < len(l.text) && (l.text[l.pos] == 'e' || l.text[l.pos] == 'E') {
		l.pos++
		if l.pos < len(l.text) && (l.text[l.pos] == '+' || l.text[l.pos] == '-') {
			l.pos++
		}
		l.scanDigits(10)
	}
	return l.numberToken(start, line)
}

// scanRadixNumber scans the digits of a 0x/0b/0o literal, including an optional
// fractional part and binary exponent for hexadecimal floats.
func (l *Lexer) scanRadixNumber(radix int) {
	l.pos += 2 // 0x / 0b / 0o
	l.scanDigits(radix)
	if radix == 16 && l.pos < len(l.text) && l.text[l.pos] == '.' && l.peek(1) != '.' {
		l.pos++
		l.scanDigits(16)
	}
	if radix == 16 && l.pos < len(l.text) && (l.text[l.pos] == 'p' || l.text[l.pos] == 'P') {
		l.pos++
		if l.pos < len(l.text) && (l.text[l.pos] == '+' || l.text[l.pos] == '-') {
			l.pos++
		}
		l.scanDigits(10)
	}
}

func (l *Lexer) scanDigits(radix int) {
	for l.pos < len(l.text) {
		c := l.text[l.pos]
		if c == '_' {
			l.pos++
			continue
		}
		if digitValue(c) < radix {
			l.pos++
			continue
		}
		return
	}
}

func digitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	default:
		return 256
	}
}

func (l *Lexer) numberToken(start, line int) Token {
	text := l.text[start:l.pos]
	value := strings.ReplaceAll(text, "_", "")
	return Token{Kind: Number, Text: text, Value: value, Pos: start, End: l.pos, Line: line}
}

func (l *Lexer) scanPunct(start, line int) Token {
	remaining := l.text[l.pos:]
	for _, punct := range multiCharPuncts {
		if strings.HasPrefix(remaining, punct) {
			l.pos += len(punct)
			return Token{Kind: Punct, Text: punct, Value: punct, Pos: start, End: l.pos, Line: line}
		}
	}
	c := l.text[l.pos]
	l.pos++
	return Token{Kind: Punct, Text: string(c), Value: string(c), Pos: start, End: l.pos, Line: line}
}
