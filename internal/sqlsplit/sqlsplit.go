// Package sqlsplit cuts a migration file into individual statements.
//
// The previous implementation handed whole files to sqloader, which split them
// for it. Go has no equivalent, so 0008-L 2.8 fixed the splitting rule itself:
// a semicolon separates statements only in normal state, never inside a string
// literal, a quoted identifier or a comment.
//
// The rule has a deliberate limit. A `BEGIN … END` compound body (trigger,
// stored procedure) contains semicolons that are statement separators to the
// database but not to this splitter, so the splitter would cut the body apart.
// Rather than grow a parser, 0008-L 2.8 forbids compound statements in the
// embedded scripts and requires startup to check for them — see BareBegin. The
// check is what makes the limit safe: a future migration that violates it stops
// the process at startup instead of being executed in pieces.
package sqlsplit

import "strings"

// state is where the scanner is: which construct, if any, currently swallows
// the characters it sees.
type state int

const (
	normal state = iota
	singleQuote
	doubleQuote
	bracket
	backtick
	lineComment
	blockComment
)

// Statements splits text and returns only the statements that contain
// something executable. 0008-L 2.8.
//
// Separators are dropped: each returned statement is the text between them,
// with surrounding whitespace left intact so that error messages and the
// statements actually executed match the file.
func Statements(text string) []string {
	var (
		out   []string
		buf   strings.Builder
		st    = normal
		runes = []rune(text)
	)
	// Working in runes rather than bytes costs one conversion and removes a
	// whole class of question: no multi-byte character can be mistaken for a
	// quote or a semicolon.
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		var next rune
		if i+1 < len(runes) {
			next = runes[i+1]
		}

		switch st {
		case normal:
			switch {
			case ch == '\'':
				st = singleQuote
			case ch == '"':
				st = doubleQuote
			case ch == '[':
				st = bracket
			case ch == '`':
				st = backtick
			case ch == '-' && next == '-':
				st = lineComment
			case ch == '/' && next == '*':
				st = blockComment
			case ch == ';':
				out = append(out, buf.String())
				buf.Reset()
				continue
			}
		case singleQuote:
			// '' inside a literal is an escaped quote, not the end of one.
			// Consuming both characters together is what keeps a semicolon
			// after `'it''s'` from being read as data.
			if ch == '\'' && next == '\'' {
				buf.WriteString("''")
				i++
				continue
			}
			if ch == '\'' {
				st = normal
			}
		case doubleQuote:
			if ch == '"' && next == '"' {
				buf.WriteString(`""`)
				i++
				continue
			}
			if ch == '"' {
				st = normal
			}
		case bracket:
			if ch == ']' {
				st = normal
			}
		case backtick:
			if ch == '`' {
				st = normal
			}
		case lineComment:
			if ch == '\n' {
				st = normal
			}
		case blockComment:
			if ch == '*' && next == '/' {
				buf.WriteString("*/")
				i++
				st = normal
				continue
			}
		}

		buf.WriteRune(ch)
	}
	// Whatever follows the last separator. In a file that ends with `;` this is
	// the empty tail that 0008-L 2.8 calls out; the filter below removes it,
	// which is why 001 counts as five statements and not six.
	out = append(out, buf.String())

	executable := out[:0]
	for _, s := range out {
		if hasExecutableSQL(s) {
			executable = append(executable, s)
		}
	}
	return executable
}

// hasExecutableSQL reports whether s is more than whitespace and comments.
func hasExecutableSQL(s string) bool {
	return strings.TrimSpace(stripComments(s)) != ""
}

// BareBegin reports whether text contains the word BEGIN outside quotes and
// comments, which is the marker for the compound statements 0008-L 2.8 forbids
// in embedded scripts.
//
// It matches the word only when it stands alone, so an identifier such as
// `begin_at` or a column called `beginning` does not trip the check.
func BareBegin(text string) bool {
	stripped := stripComments(text)
	runes := []rune(stripped)
	for i := 0; i < len(runes); i++ {
		if !isWordStart(runes, i) {
			continue
		}
		end := i
		for end < len(runes) && isWordRune(runes[end]) {
			end++
		}
		if strings.EqualFold(string(runes[i:end]), "begin") {
			return true
		}
		i = end
	}
	return false
}

// stripComments removes comments and the contents of quoted constructs, so the
// callers above look only at text the database would treat as syntax. Quoted
// contents are replaced rather than deleted to keep a literal from gluing its
// neighbours into one word.
func stripComments(text string) string {
	var (
		out   strings.Builder
		st    = normal
		runes = []rune(text)
	)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		var next rune
		if i+1 < len(runes) {
			next = runes[i+1]
		}

		switch st {
		case normal:
			switch {
			case ch == '\'':
				st = singleQuote
				out.WriteRune(' ')
			case ch == '"':
				st = doubleQuote
				out.WriteRune(' ')
			case ch == '[':
				st = bracket
				out.WriteRune(' ')
			case ch == '`':
				st = backtick
				out.WriteRune(' ')
			case ch == '-' && next == '-':
				st = lineComment
				i++
				out.WriteRune(' ')
			case ch == '/' && next == '*':
				st = blockComment
				i++
				out.WriteRune(' ')
			default:
				out.WriteRune(ch)
			}
		case singleQuote:
			if ch == '\'' && next == '\'' {
				i++
				continue
			}
			if ch == '\'' {
				st = normal
			}
		case doubleQuote:
			if ch == '"' && next == '"' {
				i++
				continue
			}
			if ch == '"' {
				st = normal
			}
		case bracket:
			if ch == ']' {
				st = normal
			}
		case backtick:
			if ch == '`' {
				st = normal
			}
		case lineComment:
			if ch == '\n' {
				st = normal
				out.WriteRune('\n')
			}
		case blockComment:
			if ch == '*' && next == '/' {
				i++
				st = normal
			}
		}
	}
	return out.String()
}

func isWordRune(r rune) bool {
	return r == '_' ||
		(r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z')
}

// isWordStart reports whether position i begins a word rather than continuing
// one.
func isWordStart(runes []rune, i int) bool {
	if !isWordRune(runes[i]) {
		return false
	}
	return i == 0 || !isWordRune(runes[i-1])
}
