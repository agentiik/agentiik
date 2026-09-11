package expr

import (
	"fmt"
	"strings"
)

// Escape rewrites the selector segments CEL's parser cannot read as field names into
// CEL's own backtick form, and returns the rewritten text beside a map from each byte of
// it back to the byte of the source it came from, so that a position in an error still
// points into the text the author wrote.
//
// It exists because of one fact about the parser that is not a fact about the workflow
// language. The language's canonical expression is ${{ inputs.in.count > 0 }}, and in is
// a CEL keyword: the lexer gives it its own token, the select rule takes an identifier,
// and the parser answers with a syntax error at the dot. in is the input port the short
// form of an edge feeds, which makes that the most common expression there is. The same
// applies to true, false and null, and to any port or step name that is not a bare CEL
// identifier: a name may begin with a digit or carry a hyphen, and inputs.my-port.count
// parses as a subtraction and reports an undeclared reference to port, which is a
// misleading error rather than an honest one.
//
// The rewrite is lexical and conservative. It happens after a dot and nowhere else,
// never inside a string literal or a comment, and never before a call parenthesis, where
// the grammar takes a bare identifier and no escaped one. A segment that is already a
// bare identifier and not a reserved word is left exactly as it was written, so the text
// the parser reads is the author's text in every case but the one this exists for.
//
// Two readings are taken here, because the source is scanned before it is parsed and a
// hyphen is both a name character and an operator.
//
// A hyphen binds into a name when it sits between two names and has no space around it:
// inputs.my-port.count is one port. A hyphen followed by a digit is the operator, so
// inputs.in.count-1 stays a subtraction. A name whose hyphen is followed by a digit is
// therefore written with CEL's own backticks, which this pass leaves untouched.
//
// A subtraction whose right side is a root is written with spaces: inputs.in.count -
// vars.floor, and not count-vars.floor, which reads as one name. Every example in the
// documentation and in the fixture corpus writes it that way, and the alternative is a
// pass that guesses what the author meant.
func Escape(src string) (string, []int, error) {
	out := make([]byte, 0, len(src)+8)
	off := make([]int, 0, len(src)+8)
	copyRange := func(from, to int) {
		for k := from; k < to; k++ {
			out = append(out, src[k])
			off = append(off, k)
		}
	}
	// insert writes a byte the author did not write, attributed to the source
	// position it stands in front of, so that a position never lands outside the
	// source and never moves past what it names.
	insert := func(b byte, at int) {
		out = append(out, b)
		off = append(off, at)
	}

	// operand says whether the token just read is something a dot may select from.
	// It is what tells a selector apart from the dot inside 1.5 and from the leading
	// dot of a root-scoped identifier.
	operand := false

	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case isSpace(c):
			copyRange(i, i+1)
			i++

		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			end := len(src)
			if nl := strings.IndexByte(src[i:], '\n'); nl >= 0 {
				end = i + nl
			}
			copyRange(i, end)
			i = end

		case c == '"' || c == '\'':
			end, err := scanString(src, i, false)
			if err != nil {
				return "", nil, err
			}
			copyRange(i, end)
			i = end
			operand = true

		case c == '`':
			// An identifier the author escaped by hand. It is already what this
			// pass would produce, so it is copied and nothing is done to it.
			end := strings.IndexByte(src[i+1:], '`')
			if end < 0 {
				return "", nil, fmt.Errorf("an escaped identifier opened with a backtick at offset %d is never closed", i)
			}
			end += i + 2
			copyRange(i, end)
			i = end
			operand = true

		case isDigit(c):
			end := scanNumber(src, i)
			copyRange(i, end)
			i = end
			// A number is not something a dot selects from: a dot after one is
			// the parser's to refuse, and the text it refuses is the author's.
			operand = false

		case isIdentStart(c):
			end := i
			for end < len(src) && isIdentChar(src[end]) {
				end++
			}
			if end < len(src) && (src[end] == '"' || src[end] == '\'') {
				if raw, ok := stringPrefix(src[i:end]); ok {
					stop, err := scanString(src, end, raw)
					if err != nil {
						return "", nil, err
					}
					copyRange(i, stop)
					i = stop
					operand = true
					continue
				}
			}
			copyRange(i, end)
			i = end
			operand = true

		case c == '.' && operand:
			copyRange(i, i+1)
			i++
			if i < len(src) && src[i] == '?' { // the optional select, a.?b
				copyRange(i, i+1)
				i++
			}
			for i < len(src) && isSpace(src[i]) {
				copyRange(i, i+1)
				i++
			}
			start := i
			seg, end := scanSegment(src, start)
			if seg == "" {
				// Whatever follows the dot, the parser says it better than a
				// rewrite would.
				operand = false
				continue
			}
			if next := skipSpace(src, end); next < len(src) && src[next] == '(' {
				// A method call. The grammar takes a bare identifier here and
				// no escaped one, so the name is copied as written and a
				// hyphen after it is the operator it looks like.
				word := bareWord(src, start)
				copyRange(start, start+len(word))
				i = start + len(word)
				operand = true
				continue
			}
			if isBareIdentifier(seg) && !reservedWords[seg] {
				copyRange(start, end)
				i = end
				operand = true
				continue
			}
			insert('`', start)
			copyRange(start, end)
			insert('`', end)
			i = end
			operand = true

		case c == ')' || c == ']' || c == '}':
			copyRange(i, i+1)
			i++
			operand = true

		default:
			copyRange(i, i+1)
			i++
			operand = false
		}
	}
	// One entry past the end, so that a position naming the end of the text maps as
	// every other position does.
	off = append(off, len(src))
	return string(out), off, nil
}

// reservedWords is the parser's own list. Four of them are keywords the lexer gives
// their own token, so only those four are syntax errors after a dot; the rest are
// refused only where they stand alone. All of them are escaped anyway, because the
// backtick form means the same thing in every case and a list that is a superset cannot
// be the one that is wrong.
var reservedWords = map[string]bool{
	"as": true, "break": true, "const": true, "continue": true, "else": true,
	"false": true, "for": true, "function": true, "if": true, "import": true,
	"in": true, "let": true, "loop": true, "package": true, "namespace": true,
	"null": true, "return": true, "true": true, "var": true, "void": true,
	"while": true,
}

// scanSegment reads the name after a selector dot, binding a hyphen into it when the
// hyphen sits between two names. It returns the name and the offset just past it, or an
// empty name when what follows the dot cannot begin one.
// A name may begin with a digit, which a bare CEL identifier may not: the grammar a step
// and a port are held to is ^[A-Za-z0-9][A-Za-z0-9_-]*$. That is why the first character
// is read the same way as the rest.
func scanSegment(src string, i int) (string, int) {
	j := i
	if j >= len(src) || !isIdentChar(src[j]) {
		return "", i
	}
	for j < len(src) && isIdentChar(src[j]) {
		j++
	}
	for j+1 < len(src) && src[j] == '-' && isIdentStart(src[j+1]) {
		j++
		for j < len(src) && isIdentChar(src[j]) {
			j++
		}
	}
	return src[i:j], j
}

// bareWord reads the plain identifier at i, hyphens excluded.
func bareWord(src string, i int) string {
	j := i
	for j < len(src) && isIdentChar(src[j]) {
		j++
	}
	return src[i:j]
}

// scanString reads a string literal whole, single or triple quoted. raw says the literal
// carries the r prefix, where a backslash is a backslash and escapes nothing.
func scanString(src string, i int, raw bool) (int, error) {
	q := src[i]
	triple := string([]byte{q, q, q})
	if strings.HasPrefix(src[i:], triple) {
		for j := i + 3; j < len(src); {
			if !raw && src[j] == '\\' {
				j += 2
				continue
			}
			if src[j] == q && strings.HasPrefix(src[j:], triple) {
				return j + 3, nil
			}
			j++
		}
		return 0, fmt.Errorf("a string opened at offset %d is never closed", i)
	}
	for j := i + 1; j < len(src); {
		switch {
		case !raw && src[j] == '\\':
			j += 2
		case src[j] == q:
			return j + 1, nil
		case src[j] == '\n':
			return 0, fmt.Errorf("a string opened at offset %d runs to the end of the line without closing", i)
		default:
			j++
		}
	}
	return 0, fmt.Errorf("a string opened at offset %d is never closed", i)
}

// stringPrefix says whether the word before a quote is a literal's prefix, and whether
// that prefix makes the literal raw. The grammar has bytes carrying a raw prefix and not
// the other way round, so br is a prefix and rb is not.
func stringPrefix(word string) (raw bool, ok bool) {
	switch strings.ToLower(word) {
	case "r":
		return true, true
	case "b":
		return false, true
	case "br":
		return true, true
	}
	return false, false
}

// scanNumber reads a numeric literal whole, the dot inside a float included, so that the
// dot of 1.5 is never read as a selector.
func scanNumber(src string, i int) int {
	j := i
	if src[i] == '0' && i+1 < len(src) && (src[i+1] == 'x' || src[i+1] == 'X') {
		j = i + 2
		for j < len(src) && isHexDigit(src[j]) {
			j++
		}
	} else {
		for j < len(src) && isDigit(src[j]) {
			j++
		}
		if j+1 < len(src) && src[j] == '.' && isDigit(src[j+1]) {
			j++
			for j < len(src) && isDigit(src[j]) {
				j++
			}
		}
		if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
			k := j + 1
			if k < len(src) && (src[k] == '+' || src[k] == '-') {
				k++
			}
			if k < len(src) && isDigit(src[k]) {
				for k < len(src) && isDigit(src[k]) {
					k++
				}
				j = k
			}
		}
	}
	if j < len(src) && (src[j] == 'u' || src[j] == 'U') {
		j++
	}
	return j
}

// skipSpace gives the offset of the next byte that is not whitespace.
func skipSpace(src string, i int) int {
	for i < len(src) && isSpace(src[i]) {
		i++
	}
	return i
}

// isBareIdentifier says whether a name is what CEL's grammar calls an IDENTIFIER, which
// is the only thing a selector takes unescaped.
func isBareIdentifier(s string) bool {
	if s == "" || !isIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isIdentChar(s[i]) {
			return false
		}
	}
	return true
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexDigit(c byte) bool {
	return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func isLetter(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }

func isIdentStart(c byte) bool { return isLetter(c) || c == '_' }

func isIdentChar(c byte) bool { return isIdentStart(c) || isDigit(c) }

// sourceOffset maps a byte offset in the escaped text back to a byte offset in the
// source. An offset past the end maps to the end, so that a caller need not bound check
// a position a compiler handed it.
func sourceOffset(offsets []int, at int) int {
	switch {
	case len(offsets) == 0:
		return 0
	case at < 0:
		return offsets[0]
	case at >= len(offsets):
		return offsets[len(offsets)-1]
	default:
		return offsets[at]
	}
}
