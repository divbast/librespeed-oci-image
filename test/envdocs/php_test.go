// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package envdocs

// A scanner for just enough PHP to find the calls that read the
// environment, written here rather than borrowed from PHP's own tokenizer
// so the test needs no PHP. It knows where code is and where it is not:
// text outside the PHP tags, comments, and the inside of strings,
// heredocs and nowdocs, apart from the {$…} and ${…} a string
// interpolates, which are code. It knows which function each call is
// written in, by the braces around it. It knows no more of PHP's grammar
// than that, and it refuses what it cannot read to the end, such as a
// string that never closes, rather than skipping the rest of a file.
//
// What it does not see is a reader called by another name: through a
// variable, call_user_func, or a namespace's use function. The image's PHP
// does none of these, and none would get past a reviewer unremarked.

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// readers are the functions that read one variable, as PHP compares
// function names: in lower case. envBool and envString are the
// bootstrap's own, and take the name as their first argument.
var readers = []string{"getenv", "envbool", "envstring"}

// superglobals are the variables that hold the environment, which PHP
// compares by case: $_ENV, and $_SERVER, which the server fills with it.
// Reading one is indexing it, and the index is the name read.
var superglobals = []string{"$_ENV", "$_SERVER"}

// plainName is a variable name the README can name: a literal argument
// with any other content, an escape among it, hides the name.
var plainName = regexp.MustCompile(`\A[A-Za-z_][A-Za-z0-9_]*\z`)

// read is one call of a reader.
type read struct {
	line int
	// function is the function the call is written in, "" at the top
	// level: a method as Class::method, and a closure as {closure}, PHP's
	// own name for one.
	function string
	// call is the call as written, from the reader's name to its closing
	// parenthesis, without white space; for $_ENV or $_SERVER, the
	// variable to the bracket that closes its index.
	call string
	// name is the variable read when the first argument is a literal that
	// names it, and "" otherwise.
	name string
}

func (r read) String() string {
	in := "at the top level"
	if r.function != "" {
		in = "in " + r.function + "()"
	}
	if r.name == "" {
		return fmt.Sprintf("line %d, %s: %s, a name not written there", r.line, in, r.call)
	}
	return fmt.Sprintf("line %d, %s: %s reads %s", r.line, in, r.call, r.name)
}

// kind is what a token is.
type kind int

const (
	word     kind = iota // a name or keyword, qualified or not
	variable             // $name
	literal              // a quoted string
	number
	symbol // any other character, or one of the operators in symbols
)

// symbols are the operators of more than one character the scanner
// needs whole, longest first.
var symbols = []string{"?->", "->", "::", "...", "#["}

type token struct {
	kind       kind
	text       string
	start, end int // offsets into the source
	// interpolated is true for a literal that interpolates a variable, and
	// so has no value the source spells out.
	interpolated bool
}

// is reports whether t is the symbol or word s, as PHP compares words: in
// any case.
func (t token) is(s string) bool {
	return (t.kind == symbol || t.kind == word) && strings.EqualFold(t.text, s)
}

type lexer struct {
	src    []byte
	pos    int
	tokens []token
	lines  []int // the offset each line starts at
}

// line is the line offset is on, counted from 1.
func (l *lexer) line(offset int) int {
	n, _ := slices.BinarySearch(l.lines, offset+1)
	return n
}

// fail is an error at offset.
func (l *lexer) fail(offset int, what string) error {
	return fmt.Errorf("line %d: %s", l.line(offset), what)
}

func (l *lexer) emit(k kind, start int) {
	l.tokens = append(l.tokens, token{kind: k, text: string(l.src[start:l.pos]), start: start, end: l.pos})
}

// lex splits src into the tokens of its code, between PHP's tags.
func lex(src []byte) (*lexer, error) {
	l := &lexer{src: src, lines: []int{0}}
	for i, c := range src {
		if c == '\n' {
			l.lines = append(l.lines, i+1)
		}
	}
	for l.pos < len(src) {
		at, n := openTag(src[l.pos:])
		if at < 0 {
			break
		}
		l.pos += at + n
		if err := l.code(false); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// openTag finds the first tag in text that opens PHP, and its length.
func openTag(text []byte) (at, n int) {
	for i := 0; i+2 <= len(text); i++ {
		if !bytes.HasPrefix(text[i:], []byte("<?")) {
			continue
		}
		rest := text[i+2:]
		if bytes.HasPrefix(rest, []byte("=")) {
			return i, 3
		}
		if len(rest) >= 3 && strings.EqualFold(string(rest[:3]), "php") && (len(rest) == 3 || isSpace(rest[3])) {
			return i, 5
		}
	}
	return -1, 0
}

// code lexes code from l.pos: to a closing tag or the end of the source,
// or, when interpolation is true, to the brace that closes the
// interpolation it is in.
func (l *lexer) code(interpolation bool) error {
	opened := l.pos
	depth := 0
	for l.pos < len(l.src) {
		start, c, rest := l.pos, l.src[l.pos], l.src[l.pos:]
		switch {
		case isSpace(c):
			l.pos++
		case bytes.HasPrefix(rest, []byte("?>")):
			if interpolation {
				return l.fail(opened, "an interpolation that never ends")
			}
			l.pos += 2
			return nil
		case c == '#' && !bytes.HasPrefix(rest, []byte("#[")) || bytes.HasPrefix(rest, []byte("//")):
			l.lineComment()
		case bytes.HasPrefix(rest, []byte("/*")):
			end := bytes.Index(rest[2:], []byte("*/"))
			if end < 0 {
				return l.fail(start, "a comment that never ends")
			}
			l.pos += 2 + end + 2
		case c == '\'':
			if err := l.single(); err != nil {
				return err
			}
		case c == '"' || c == '`':
			if err := l.double(c); err != nil {
				return err
			}
		case bytes.HasPrefix(rest, []byte("<<<")):
			if err := l.heredoc(); err != nil {
				return err
			}
		case c == '$' && len(rest) > 1 && isNameStart(rest[1]):
			l.pos++
			l.name()
			l.emit(variable, start)
		case isNameStart(c) || (c == '\\' && len(rest) > 1 && isNameStart(rest[1])):
			l.name()
			l.emit(word, start)
		case isDigit(c):
			for l.pos < len(l.src) && (isNameChar(l.src[l.pos]) || l.src[l.pos] == '.') {
				l.pos++
			}
			l.emit(number, start)
		case interpolation && c == '}' && depth == 0:
			l.pos++
			return nil
		default:
			n := 1
			if i := slices.IndexFunc(symbols, func(s string) bool { return bytes.HasPrefix(rest, []byte(s)) }); i >= 0 {
				n = len(symbols[i])
			}
			switch c {
			case '{':
				depth++
			case '}':
				depth--
			}
			l.pos += n
			l.emit(symbol, start)
		}
	}
	if interpolation {
		return l.fail(opened, "an interpolation that never ends")
	}
	return nil
}

// name skips a name, qualified with backslashes or not.
func (l *lexer) name() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '\\' && l.pos+1 < len(l.src) && isNameStart(l.src[l.pos+1]) || isNameChar(c) {
			l.pos++
			continue
		}
		return
	}
}

// lineComment skips a comment to the end of its line, or to a closing tag,
// which ends PHP even there.
func (l *lexer) lineComment() {
	for l.pos < len(l.src) && l.src[l.pos] != '\n' && !bytes.HasPrefix(l.src[l.pos:], []byte("?>")) {
		l.pos++
	}
}

// single lexes a single-quoted string, which interpolates nothing.
func (l *lexer) single() error {
	start := l.pos
	for l.pos++; l.pos < len(l.src); l.pos++ {
		switch l.src[l.pos] {
		case '\\':
			l.pos++
		case '\'':
			l.pos++
			l.emit(literal, start)
			return nil
		}
	}
	return l.fail(start, "a string that never ends")
}

// double lexes a double-quoted or backquoted string, and the code each of
// its interpolations holds.
func (l *lexer) double(quote byte) error {
	start := l.pos
	l.pos++
	l.emit(literal, start)
	at := len(l.tokens) - 1
	interpolated, closed, err := l.interpolate(len(l.src), quote)
	if err != nil {
		return err
	}
	if !closed {
		return l.fail(start, "a string that never ends")
	}
	l.finish(at, interpolated)
	return nil
}

// interpolate lexes a string's body from l.pos, up to end or past quote,
// whichever comes first, as code each {$…} and ${…} in it. It reports
// whether the string interpolates anything, and whether it found quote.
// The string's own token is already emitted, so the tokens of its code
// follow it.
func (l *lexer) interpolate(end int, quote byte) (interpolated, closed bool, err error) {
	for l.pos < end {
		rest := l.src[l.pos:end]
		switch {
		case quote != 0 && rest[0] == quote:
			l.pos++
			return interpolated, true, nil
		// { cannot be escaped: \{$ is a backslash, then an interpolation
		case rest[0] == '\\' && len(rest) > 1 && rest[1] != '{':
			l.pos += 2
		case bytes.HasPrefix(rest, []byte("{$")), bytes.HasPrefix(rest, []byte("${")):
			interpolated = true
			l.pos += 1 + strings.IndexByte(string(rest[:2]), '{')
			if err := l.code(true); err != nil {
				return false, false, err
			}
		case rest[0] == '$' && len(rest) > 1 && isNameStart(rest[1]):
			interpolated = true
			l.pos++
		default:
			l.pos++
		}
	}
	return interpolated, false, nil
}

// finish completes the string token at index at, which ends at l.pos.
func (l *lexer) finish(at int, interpolated bool) {
	t := &l.tokens[at]
	t.text, t.end, t.interpolated = string(l.src[t.start:l.pos]), l.pos, interpolated
}

// label is a heredoc's opening line: <<<, an optional quote, and the label
// that ends it.
var label = regexp.MustCompile(`\A<<<[ \t]*(?:"([A-Za-z_][A-Za-z0-9_]*)"|'([A-Za-z_][A-Za-z0-9_]*)'|([A-Za-z_][A-Za-z0-9_]*))\r?\n`)

// heredoc lexes a heredoc, which interpolates as a double-quoted string
// does, or a nowdoc, which interpolates nothing. It ends at the first line
// that starts, after any indentation, with its label and no more of a
// name.
func (l *lexer) heredoc() error {
	start := l.pos
	m := label.FindSubmatch(l.src[l.pos:])
	if m == nil {
		return l.fail(start, "<<< opens no heredoc")
	}
	name, nowdoc := m[3], false
	switch {
	case m[1] != nil:
		name = m[1]
	case m[2] != nil:
		name, nowdoc = m[2], true
	}
	body := l.pos + len(m[0])
	end, after := -1, 0
	for i := body; i < len(l.src); {
		line := l.src[i:]
		indent := len(line) - len(bytes.TrimLeft(line, " \t"))
		if rest := line[indent:]; bytes.HasPrefix(rest, name) && (len(rest) == len(name) || !isNameChar(rest[len(name)])) {
			end, after = i, i+indent+len(name)
			break
		}
		next := bytes.IndexByte(line, '\n')
		if next < 0 {
			break
		}
		i += next + 1
	}
	if end < 0 {
		return l.fail(start, "a heredoc that never ends")
	}
	l.pos = body
	l.emit(literal, start)
	at := len(l.tokens) - 1
	interpolated := false
	if !nowdoc {
		var err error
		if interpolated, _, err = l.interpolate(end, 0); err != nil {
			return err
		}
	}
	l.pos = after
	l.finish(at, interpolated)
	return nil
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}
func isDigit(c byte) bool     { return '0' <= c && c <= '9' }
func isNameStart(c byte) bool { return c == '_' || ('a' <= c|0x20 && c|0x20 <= 'z') || c >= 0x80 }
func isNameChar(c byte) bool  { return isNameStart(c) || isDigit(c) }

// scope is a function or class whose body is open.
type scope struct {
	name  string
	class bool
	depth int // the brace depth inside its body
}

// reads returns every call of a reader in src, in the order written.
func reads(src []byte) ([]read, error) {
	l, err := lex(src)
	if err != nil {
		return nil, err
	}
	at := func(i int) token {
		if i < 0 || i >= len(l.tokens) {
			return token{kind: symbol}
		}
		return l.tokens[i]
	}

	var (
		found   []read
		open    []scope
		pending *scope // a declaration whose body has not opened yet
		depth   int
	)
	for i, t := range l.tokens {
		prev, next := at(i-1), at(i+1)
		member := prev.is("->") || prev.is("?->") || prev.is("::")
		switch {
		case t.is("{"):
			depth++
			if pending != nil {
				pending.depth = depth
				open = append(open, *pending)
				pending = nil
			}
		case t.is("}"):
			if n := len(open); n > 0 && open[n-1].depth == depth {
				open = open[:n-1]
			}
			depth--
		case t.is(";"):
			// A method declared without a body
			pending = nil
		case t.is("function") && !member:
			name := next
			if name.is("&") {
				name = at(i + 2)
			}
			s := scope{name: "{closure}"}
			if name.kind == word {
				s.name = name.text
				if n := len(open); n > 0 && open[n-1].class {
					s.name = open[n-1].name + "::" + s.name
				}
			}
			pending = &s
		case (t.is("class") || t.is("interface") || t.is("trait") || t.is("enum")) && !member:
			switch {
			case t.is("class") && (prev.is("new") || prev.is("readonly") && at(i-2).is("new")):
				pending = &scope{name: "class@anonymous", class: true}
			case next.kind == word:
				pending = &scope{name: next.text, class: true}
			}
		case t.kind == variable && slices.Contains(superglobals, t.text) && next.is("[") && !member:
			r, err := l.call(i)
			if err != nil {
				return nil, err
			}
			if n := len(open); n > 0 {
				r.function = open[n-1].name
			}
			found = append(found, r)
		case t.kind == word && slices.Contains(readers, strings.ToLower(strings.TrimPrefix(t.text, `\`))) &&
			next.is("(") && !member && !prev.is("function") && !prev.is("new") &&
			(!prev.is("&") || !at(i-2).is("function")):
			r, err := l.call(i)
			if err != nil {
				return nil, err
			}
			if n := len(open); n > 0 {
				r.function = open[n-1].name
			}
			found = append(found, r)
		}
	}
	return found, nil
}

// call reads the call whose name is l.tokens[i], and whose opening
// parenthesis follows it, or the index of the superglobal that is
// l.tokens[i], whose opening bracket follows it.
func (l *lexer) call(i int) (read, error) {
	var first []token // the first argument's tokens
	inFirst, depth := true, 0
	for j := i + 1; j < len(l.tokens); j++ {
		t := l.tokens[j]
		switch {
		case t.is("(") || t.is("[") || t.is("{") || t.is("#["):
			depth++
		case t.is(")") || t.is("]") || t.is("}"):
			depth--
		}
		switch {
		case depth == 0:
			return l.read(i, j, first), nil
		case j == i+1:
			// The opening parenthesis
		case depth == 1 && t.is(","):
			inFirst = false
		case inFirst:
			first = append(first, t)
		}
	}
	return read{}, l.fail(l.tokens[i].start, "a call that never ends")
}

// read is the call from l.tokens[i] to the parenthesis that closes it at
// l.tokens[closed], whose first argument, or index, is first.
func (l *lexer) read(i, closed int, first []token) read {
	r := read{
		line: l.line(l.tokens[i].start),
		call: strings.Join(strings.Fields(string(l.src[l.tokens[i].start:l.tokens[closed].end])), ""),
	}
	if len(first) != 1 || first[0].kind != literal || first[0].interpolated {
		return r
	}
	text := first[0].text
	if q := text[0]; (q == '\'' || q == '"') && len(text) >= 2 && text[len(text)-1] == q &&
		plainName.MatchString(text[1:len(text)-1]) {
		r.name = text[1 : len(text)-1]
	}
	return r
}
