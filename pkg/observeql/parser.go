package observeql

import (
	"fmt"
	"strings"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

// observeqlLexer defines a small lexer that recognises:
//
//   - SQL-ish identifiers (a-z A-Z _, then alnum + . + _)
//   - integers (Int)
//   - floats (Number)
//   - strings ("..." or '...', with backslash escapes)
//   - duration tokens like 30m, 1h, 750ms
//   - the operators we need
//
// We keep all keywords case-insensitive by upper-casing them before
// matching in the parser. participle's StatelessLexer wants regex per
// token; the order below is significant — Duration must be matched
// before Number so "30m" doesn't parse as 30 followed by ident "m".
var observeqlLexer = lexer.MustSimple([]lexer.SimpleRule{
	{Name: "whitespace", Pattern: `[\s]+`},
	{Name: "Duration", Pattern: `[0-9]+(ms|s|m|h|d)\b`},
	// Single Number token covers both ints and floats; the AST uses
	// float64 and we round-trip to int when the AST field is an int.
	{Name: "Number", Pattern: `[-+]?\d+(\.\d+)?`},
	{Name: "String", Pattern: `"(\\.|[^"])*"|'(\\.|[^'])*'`},
	{Name: "Ident", Pattern: `[a-zA-Z_][a-zA-Z0-9_.]*`},
	{Name: "Punct", Pattern: `[(),*=<>!]`},
})

var queryParser = participle.MustBuild[Query](
	participle.Lexer(observeqlLexer),
	participle.CaseInsensitive("Ident"),
	participle.Unquote("String"),
	participle.UseLookahead(2),
)

// Parse parses an ObserveQL query string into an AST. Errors include
// a line+column position pointing at the failed token, suitable for
// surfacing back to the API caller.
func Parse(input string) (*Query, error) {
	q, err := queryParser.ParseString("", input)
	if err != nil {
		return nil, fmt.Errorf("observeql: %w", err)
	}
	if err := q.normalise(); err != nil {
		return nil, err
	}
	return q, nil
}

func (q *Query) normalise() error {
	if q.From == nil {
		return fmt.Errorf("observeql: missing FROM clause")
	}
	q.From.Source = strings.ToLower(q.From.Source)
	switch q.From.Source {
	case "metrics", "logs", "traces":
	default:
		return fmt.Errorf("observeql: unknown source %q", q.From.Source)
	}

	if q.Select == nil {
		return fmt.Errorf("observeql: missing SELECT clause")
	}
	if !q.Select.Star && len(q.Select.Columns) == 0 {
		return fmt.Errorf("observeql: SELECT requires either * or at least one column")
	}
	return nil
}

// Source is a small convenience so callers don't have to navigate the
// AST to learn what table is being queried.
func (q *Query) Source() string { return q.From.Source }
