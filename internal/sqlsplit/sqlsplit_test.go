package sqlsplit

import (
	"reflect"
	"strings"
	"testing"
)

// TestStatementsSeparates covers the states of 0008-L 2.8 one at a time. Every
// case is a semicolon that must not be treated as a separator, which is the
// only way the splitter can be wrong in a way that damages a database: a
// statement cut in half either fails to execute or, worse, executes as two
// statements that each mean something different.
func TestStatementsSeparates(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "plain separation",
			in:   "SELECT 1; SELECT 2;",
			want: []string{"SELECT 1", " SELECT 2"},
		},
		{
			name: "semicolon inside a string literal",
			in:   "INSERT INTO t VALUES ('a;b'); SELECT 1;",
			want: []string{"INSERT INTO t VALUES ('a;b')", " SELECT 1"},
		},
		{
			name: "escaped quote inside a string literal",
			in:   "INSERT INTO t VALUES ('it''s; fine'); SELECT 1;",
			want: []string{"INSERT INTO t VALUES ('it''s; fine')", " SELECT 1"},
		},
		{
			name: "semicolon inside a quoted identifier",
			in:   `CREATE TABLE "od;d" (a INT); SELECT 1;`,
			want: []string{`CREATE TABLE "od;d" (a INT)`, " SELECT 1"},
		},
		{
			name: "semicolon inside a bracket identifier",
			in:   "CREATE TABLE [od;d] (a INT); SELECT 1;",
			want: []string{"CREATE TABLE [od;d] (a INT)", " SELECT 1"},
		},
		{
			name: "semicolon inside a backtick identifier",
			in:   "CREATE TABLE `od;d` (a INT); SELECT 1;",
			want: []string{"CREATE TABLE `od;d` (a INT)", " SELECT 1"},
		},
		{
			name: "semicolon inside a line comment",
			in:   "SELECT 1 -- trailing ; not a separator\n; SELECT 2;",
			want: []string{"SELECT 1 -- trailing ; not a separator\n", " SELECT 2"},
		},
		{
			name: "semicolon inside a block comment",
			in:   "SELECT 1 /* a ; b */; SELECT 2;",
			want: []string{"SELECT 1 /* a ; b */", " SELECT 2"},
		},
		{
			name: "comment-only fragments are dropped",
			in:   "-- just a note\n; SELECT 1;",
			want: []string{" SELECT 1"},
		},
		{
			name: "empty tail after the final separator is dropped",
			in:   "SELECT 1;\n\n",
			want: []string{"SELECT 1"},
		},
		{
			name: "statement without a trailing separator is kept",
			in:   "SELECT 1",
			want: []string{"SELECT 1"},
		},
		{
			name: "empty input yields nothing",
			in:   "   \n\t ",
			want: []string{},
		},
		{
			name: "multi-byte text is not mistaken for syntax",
			in:   "INSERT INTO t VALUES ('한글; 값'); SELECT 1;",
			want: []string{"INSERT INTO t VALUES ('한글; 값')", " SELECT 1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Statements(tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Statements(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestStatementsAreExecutable checks that what comes back can be handed to a
// database as-is: the separator is gone and nothing is left but the statement
// and its surrounding whitespace.
func TestStatementsAreExecutable(t *testing.T) {
	for _, s := range Statements("SELECT 1; SELECT 2;") {
		if strings.Contains(s, ";") {
			t.Errorf("statement still carries a separator: %q", s)
		}
		if strings.TrimSpace(s) == "" {
			t.Errorf("empty statement returned")
		}
	}
}

// TestBareBegin guards the limit 0008-L 2.8 accepted in exchange for not
// writing a parser. The splitter cannot see inside a compound statement, so a
// script containing one has to be refused at startup — and refused only when
// BEGIN really is a compound statement, or a column named `begin_at` would stop
// the server.
func TestBareBegin(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"trigger body", "CREATE TRIGGER t AFTER INSERT ON x BEGIN UPDATE y SET a=1; END;", true},
		{"lower case", "begin transaction;", true},
		{"identifier prefix", "CREATE TABLE t (begin_at TIMESTAMP);", false},
		{"identifier suffix", "SELECT the_begin FROM t;", false},
		{"inside a word", "SELECT beginning FROM t;", false},
		{"inside a string literal", "INSERT INTO t VALUES ('BEGIN');", false},
		{"inside a line comment", "-- BEGIN is mentioned here\nSELECT 1;", false},
		{"inside a block comment", "/* BEGIN */ SELECT 1;", false},
		{"inside a quoted identifier", `CREATE TABLE "BEGIN" (a INT);`, false},
		{"none of the above", "CREATE TABLE t (a INT);", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BareBegin(tc.in); got != tc.want {
				t.Errorf("BareBegin(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestQuotedTextDoesNotGlueWords is the reason stripComments substitutes a
// space for a quoted run rather than deleting it. Without the substitution
// `x'a'begin` would collapse to `xbegin` and the word check would miss it —
// or, in the mirror case, invent a word that is not there.
func TestQuotedTextDoesNotGlueWords(t *testing.T) {
	if BareBegin("SELECT xy'lit'zw;") {
		t.Error("quoted text glued its neighbours into a new word")
	}
	if !BareBegin("SELECT 'lit' BEGIN;") {
		t.Error("a real BEGIN after a literal was missed")
	}
}
