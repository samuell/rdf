package parse

import (
	"bytes"
	"fmt"
	"strconv"
)

type tokenType int

const (
	// special tokens
	tokenNone  tokenType = iota
	tokenEOL             // end of line
	tokenError           // an illegal token

	// turtle tokens
	tokenIRIAbs         // RDF IRI reference (absolute)
	tokenIRIRel         // RDF IRI reference (relative)
	tokenBNode          // RDF blank node
	tokenLiteral        // RDF literal
	tokenLangMarker     // '@''
	tokenLang           // literal language tag
	tokenDataTypeMarker // '^^''
	tokenDot            // '.''
	tokenRDFType        // 'a' => <http://www.w3.org/1999/02/22-rdf-syntax-ns#type>
	tokenPrefix         // @prefix
	tokenPrefixLabel    // @prefix tokenPrefixLabel: IRI
)

const eof = -1

// Rune helper values and functions:
var (
	hex            = []byte("0123456789ABCDEFabcdef")
	badIRIRunes    = [...]rune{' ', '<', '"', '{', '}', '|', '^', '`'}
	okAfterRDFType = [...]rune{' ', '\t', '<', '"', '\''}
	pnTab          = []rune{
		'A', 'Z',
		'a', 'z',
		0x00C0, 0x00D6,
		0x00D8, 0x00F6,
		0x00F8, 0x02FF,
		0x0370, 0x037D,
		0x037F, 0x1FFF,
		0x200C, 0x200D,
		0x2070, 0x218F,
		0x2C00, 0x2FEF,
		0x3001, 0xD7FF,
		0xF900, 0xFDCF,
		0xFDF0, 0xFFFD,
		0x10000, 0xEFFFF, // last of PN_CHARS_BASE
		'_', '_',
		':', ':', // last of PN_CHARS_U
		'-', '-',
		'0', '9',
		0x00B7, 0x00B7,
		0x0300, 0x036F,
		0x203F, 0x2040, // last of PN_CHARS
	}
)

func isAlpha(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isDigit(r rune) bool {
	return (r >= '0' && r <= '9')
}

func isAlphaOrDigit(r rune) bool {
	return isAlpha(r) || isDigit(r)
}

func check(r rune, tab []rune) bool {
	for i := 0; i < len(tab); i += 2 {
		if r >= tab[i] && r <= tab[i+1] {
			return true
		}
	}
	return false
}

func isPnCharsBase(r rune) bool {
	return check(r, pnTab[:2*14])
}

func isPnCharsU(r rune) bool {
	return check(r, pnTab[:2*16])
}

func isPnChars(r rune) bool {
	return check(r, pnTab)
}

// token represents a token emitted by the lexer.
type token struct {
	typ  tokenType // type of token
	line int       // line number
	col  int       // column number (measured in runes, not bytes)
	text string    // the value of the token
}

// stateFn represents the state of the lexer as a function that returns the next state.
type stateFn func(*lexer) stateFn

// lexer for trig/turtle (and their line-based subsets n-triples & n-quads).
//
// The lexer is assumed to be working on one line at a time. When end of line
// is reached, tokenEOL is emitted, and the caller may supply more lines to
// the incoming channel. If there are no more input to be scanned, the user (parser)
// must call stop(), which will terminate lexing goroutine.
//
// Tokens for whitespace and comments are not emitted.
//
// The design of the lexer and indeed much of the implementation is lifted from
// the template lexer in Go's standard library, and is governed by a BSD licence
// and Copyright 2011 The Go Authors.
type lexer struct {
	incoming chan []byte

	input  []byte     // the input being scanned (should not inlcude newlines)
	state  stateFn    // the next lexing function to enter
	line   int        // the current line number
	pos    int        // the current position in input
	width  int        // width of the last rune read from input
	start  int        // start of current token
	unEsc  bool       // true when current token needs to be unescaped
	tokens chan token // channel of scanned tokens
}

func newLexer() *lexer {
	l := lexer{
		incoming: make(chan []byte),
		tokens:   make(chan token),
	}
	go l.run()
	return &l
}

// next returns the next rune in the input.
func (l *lexer) next() rune {
	if l.pos >= len(l.input) {
		l.width = 0
		return eof
	}
	r, w := decodeRune(l.input[l.pos:])
	l.width = w
	l.pos += l.width
	return r
}

// peek returns but does not consume the next rune in the input.
func (l *lexer) peek() rune {
	r := l.next()
	l.backup()
	return r
}

// backup steps back one rune. Can only be called once per call of next.
func (l *lexer) backup() {
	l.pos -= l.width
}

func (l *lexer) unescape(s string) string {
	if !l.unEsc {
		return s
	}
	r := []rune(s)
	buf := bytes.NewBuffer(make([]byte, 0, len(r)))

	for i := 0; i < len(r); {
		switch r[i] {
		case '\\':
			i++
			var c byte
			switch r[i] {
			case 't':
				c = '\t'
			case 'b':
				c = '\b'
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 'f':
				c = '\f'
			case '"':
				c = '"'
			case '\'':
				c = '\''
			case '\\':
				c = '\\'
			case 'u':
				rc, _ := strconv.ParseInt(string(r[i+1:i+5]), 16, 32)
				// we can safely assume no error, because we allready veryfied
				// the escape sequence in the lex state funcitons
				buf.WriteRune(rune(rc))
				i += 5
				continue
			case 'U':
				rc, _ := strconv.ParseInt(string(r[i+1:i+9]), 16, 32)
				// we can safely assume no error, because we allready veryfied
				// the escape sequence in the lex state funcitons
				buf.WriteRune(rune(rc))
				i += 9
				continue
			}
			buf.WriteByte(c)
		default:
			buf.WriteRune(r[i])
		}
		i++
	}
	l.unEsc = false
	return buf.String()
}

// emit publishes a token back to the comsumer.
func (l *lexer) emit(typ tokenType) {
	l.tokens <- token{
		typ:  typ,
		line: l.line,
		col:  l.start,
		text: l.unescape(string(l.input[l.start:l.pos])),
	}
	l.start = l.pos
}

// ignore skips over the pending input before this point.
func (l *lexer) ignore() {
	l.start = l.pos
}

// acceptRunMin consumes a run of runes from the valid set, returning
// true if a minimum number of runes where consumed.
func (l *lexer) acceptRunMin(valid []byte, num int) bool {
	c := 0
	for bytes.IndexRune(valid, l.next()) >= 0 {
		c++
	}
	l.backup()
	return c >= num
}

// acceptExact consumes the given string in l.input and returns true,
// or otherwise false if the string is not matched in l.input.
// The string must not contain multi-byte runes.
func (l *lexer) acceptExact(s string) bool {
	if len(l.input[l.start:]) < len(s) {
		return false
	}
	if string(l.input[l.start:l.pos+len(s)-1]) == s {
		l.pos = l.pos + len(s) - 1
		return true
	}
	return false
}

// nextToken returns the next token from the input.
func (l *lexer) nextToken() token {
	token := <-l.tokens
	return token
}

// run runs the state machine for the lexer.
func (l *lexer) run() {
again:
	line := <-l.incoming // <- blocking
	if line == nil {
		// incoming channel is closed; terminate lexer
		return
	}
	l.input = line
	l.pos = 0
	l.start = 0
	l.line++
	for l.state = lexAny; l.state != nil; {
		l.state = l.state(l)
	}
	goto again
}

func (l *lexer) stop() {
	close(l.incoming)
}

// state functions:

// errorf returns an error token and terminates the scan by passing
// back a nil pointer that will be the next state, terminating l.nextToken.
func (l *lexer) errorf(format string, args ...interface{}) stateFn {
	l.tokens <- token{
		tokenError,
		l.line,
		l.pos,
		fmt.Sprintf(format, args...),
	}
	return nil
}

func lexAny(l *lexer) stateFn {
	r := l.next()
	switch r {
	case '@':
		n := l.next()
		switch n {
		case 'p':
			l.start++ // consume '@''
			return lexPrefix
		case 'b':
			l.start++ // consume '@''
			return lexBase
		default:
			l.backup()
			return l.errorf("illegal character %q", r)
		}
	case '_':
		if l.peek() != ':' {
			return l.errorf("illegal character %q in blank node identifier", l.peek())
		}
		// consume & ignore '_:'
		l.next()
		l.ignore()
		return lexBNode
	case '<':
		l.ignore()
		return lexIRI
	case 'a':
		p := l.peek()
		for _, a := range okAfterRDFType {
			if p == a {
				l.emit(tokenRDFType)
				return lexAny
			}
		}
		return l.errorf("illegal character %q", r)
	case '"':
		l.ignore()
		return lexLiteral
	case ' ', '\t':
		// whitespace tokens are not emitted, we continue
		l.ignore()
		return lexAny
	case '.':
		l.ignore()
		l.emit(tokenDot)
		return lexAny
	case '\n':
		l.ignore()
		if l.next() != eof {
			panic("lexer got multi-line input")
		}
		l.ignore()
		l.emit(tokenEOL)
		return nil
	case '#', eof:
		l.ignore()
		l.emit(tokenEOL)
		return nil
	default:
		return l.errorf("illegal character %q", r)
	}
}

func hasValidScheme(l *lexer) bool {
	// RFC 2396: scheme = alpha *( alpha | digit | "+" | "-" | "." )

	// decode first rune, must be in set [a-zA-Z]
	r, w := decodeRune(l.input[l.start:])
	if !isAlpha(r) {
		return false
	}

	// remaining runes must be alphanumeric or '+', '-', '.''
	for p := l.start + w; p < l.pos-w; p = p + w {
		r, w = decodeRune(l.input[p:])
		if isAlphaOrDigit(r) || r == '+' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func _lexIRI(l *lexer) (stateFn, bool) {
	hasScheme := false    // does it have a scheme? defines if IRI is absolute or relative
	maybeAbsolute := true // false if we reach a non-valid scheme rune before ':'
	for {
		r := l.next()
		if r == eof {
			return l.errorf("bad IRI: no closing '>'"), false
		}
		for _, bad := range badIRIRunes {
			if r == bad {
				return l.errorf("bad IRI: disallowed character %q", r), false
			}
		}

		if r == '\\' {
			// handle numeric escape sequences for unicode points:
			esc := l.peek()
			switch esc {
			case 'u':
				l.next() // cosume 'u'
				if !l.acceptRunMin(hex, 4) {
					return l.errorf("bad IRI: insufficent hex digits in unicode escape"), false
				}
				l.unEsc = true
			case 'U':
				l.next() // cosume 'U'
				if !l.acceptRunMin(hex, 8) {
					return l.errorf("bad IRI: insufficent hex digits in unicode escape"), false
				}
				l.unEsc = true
			case eof:
				return l.errorf("bad IRI: no closing '>'"), false
			default:
				return l.errorf("bad IRI: disallowed escape character %q", esc), false
			}
		}
		if maybeAbsolute && r == ':' {
			// Check if we have an absolute IRI
			if l.pos != l.start && hasValidScheme(l) && l.peek() != eof {
				hasScheme = true
				maybeAbsolute = false // stop checking for absolute
			}
		}
		if r == '>' {
			// reached end of IRI
			break
		}
	}
	l.backup()
	return nil, hasScheme
}

func lexIRI(l *lexer) stateFn {
	res, absolute := _lexIRI(l)
	if res != nil {
		return res
	}
	if absolute {
		l.emit(tokenIRIAbs)
	} else {
		l.emit(tokenIRIRel)
	}

	// ignore '>'
	l.pos++
	l.ignore()

	return lexAny
}

func lexLiteral(l *lexer) stateFn {
	for {
		r := l.next()
		if r == eof {
			return l.errorf("bad Literal: no closing '\"'")
		}
		if r == '\\' {
			// handle numeric escape sequences for unicode points:
			esc := l.peek()
			switch esc {
			case 't', 'b', 'n', 'r', 'f', '"', '\'', '\\':
				l.next() // consume '\'
				l.unEsc = true
			case 'u':
				l.next() // cosume 'u'
				if !l.acceptRunMin(hex, 4) {
					return l.errorf("bad literal: insufficent hex digits in unicode escape")
				}
				l.unEsc = true
			case 'U':
				l.next() // cosume 'U'
				if !l.acceptRunMin(hex, 8) {
					return l.errorf("bad literal: insufficent hex digits in unicode escape")
				}
				l.unEsc = true
			case eof:
				return l.errorf("bad literal: no closing '\"'")
			default:
				return l.errorf("bad literal: disallowed escape character %q", esc)
			}
		}
		if r == '"' {
			// reached end of Literal
			break
		}
	}
	l.backup()

	l.emit(tokenLiteral)

	// ignore '"'
	l.pos++
	l.ignore()

	// check if literal has language tag or datatype URI:
	r := l.next()
	switch r {
	case '@':
		l.emit(tokenLangMarker)
		return lexLang
	case '^':
		if l.next() != '^' {
			return l.errorf("bad literal: invalid datatype IRI")
		}
		l.emit(tokenDataTypeMarker)
		if l.next() != '<' {
			return l.errorf("bad literal: invalid datatype IRI")
		}
		l.ignore() // ignore '<'
		return lexIRI
	case ' ', '\t':
		return lexAny
	default:
		l.backup()
		return lexAny
	}
}

func lexBNode(l *lexer) stateFn {
	r := l.next()
	if r == eof {
		return l.errorf("bad blank node: unexpected end of line")
	}
	if !(isPnCharsU(r) || isDigit(r)) {
		return l.errorf("bad blank node: invalid character %q", r)
	}

	for {
		r = l.next()

		if r == '.' {
			// Blank node labels can include '.', except as the final character
			if isPnChars(l.peek()) {
				continue
			} else {
				l.pos-- // backup, '.' has width 1
				break
			}
		}

		if !(isPnChars(r)) {
			l.backup()
			break
		}
	}
	l.emit(tokenBNode)
	return lexAny
}

func lexLang(l *lexer) stateFn {
	// consume [A-Za-z]+
	c := 0
	for r := l.next(); isAlpha(r); r = l.next() {
		c++
	}
	l.backup()
	if c == 0 {
		return l.errorf("bad literal: invalid language tag")
	}

	// consume ('-' [A-Za-z0-9])* if present
	if l.peek() == '-' {
		l.next() // consume '-'

		c = 0
		for r := l.next(); isAlphaOrDigit(r); r = l.next() {
			c++
		}
		l.backup()
		if c == 0 {
			return l.errorf("bad literal: invalid language tag")
		}
	}

	l.emit(tokenLang)
	return lexAny
}

func lexPrefix(l *lexer) stateFn {
	if l.acceptExact("prefix") {
		l.emit(tokenPrefix)
		// consume and ignore any whitespace before localname
		for r := l.next(); r == ' ' || r == '\t'; r = l.next() {
		}
		l.backup()
		l.ignore()
		return lexPrefixLabel
	}
	return l.errorf("invalid character 'p'")
}

func lexPrefixLabel(l *lexer) stateFn {
	r := l.next()
	if r == ':' {
		//PN_PREFIX can be empty
		l.emit(tokenPrefixLabel)
		return lexAny
	}
	if !isPnCharsBase(r) {
		return l.errorf("invalid character %q in prefix label", r)
	}
	for {
		r = l.next()
		if r == ':' {
			l.backup()
			break
		}
		if !(isPnChars(r) || (r == '.' && l.peek() != ':')) {
			return l.errorf("invalid character %q in prefix label", r)
		}
	}

	l.emit(tokenPrefixLabel)

	// consume and ignore ':'
	l.next()
	l.ignore()

	return lexAny
}

func lexBase(l *lexer) stateFn {
	return l.errorf("not implemented")
}
