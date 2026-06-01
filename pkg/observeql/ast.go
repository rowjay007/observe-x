// Package observeql parses the ObserveQL surface query language and
// lowers it to ClickHouse SQL.
//
// ObserveQL grammar (Phase B-3 subset — full spec lands in Phase C):
//
//	query    := SELECT (STAR | columns) FROM source [where] [groupby] [since] [limit]
//	source   := METRICS | LOGS | TRACES
//	where    := WHERE expr
//	groupby  := GROUP BY ident (',' ident)*
//	since    := SINCE duration
//	limit    := LIMIT integer
//
//	expr     := orExpr
//	orExpr   := andExpr (OR andExpr)*
//	andExpr  := cmp (AND cmp)*
//	cmp      := value op value | '(' expr ')' | NOT cmp
//	op       := '=' | '!=' | '<' | '<=' | '>' | '>='
//	value    := ident | string | number
//
//	duration := /[0-9]+(ms|s|m|h|d)/
//	ident    := /[a-zA-Z_][a-zA-Z0-9_.]*/
//
// Examples:
//
//	SELECT * FROM logs WHERE severity = "ERROR" SINCE 1h
//	SELECT service, count(*) FROM traces WHERE duration_ms > 1000
//	  GROUP BY service SINCE 30m LIMIT 100
//
// Reserved identifiers (allow-list, enforced by the planner): see
// allowed_columns.go.
package observeql

import (
	"github.com/alecthomas/participle/v2/lexer"
)

// ─── AST shapes ───────────────────────────────────────────────────────────

type Query struct {
	Pos lexer.Position

	Select  *SelectClause  `parser:"@@"`
	From    *FromClause    `parser:"@@"`
	Where   *WhereClause   `parser:"@@?"`
	GroupBy *GroupByClause `parser:"@@?"`
	Since   *SinceClause   `parser:"@@?"`
	Limit   *LimitClause   `parser:"@@?"`
}

type SelectClause struct {
	Star    bool      `parser:"'SELECT' (@'*'"`
	Columns []*Column `parser:"| @@ ( ',' @@ )* )"`
}

type Column struct {
	// Either a function call like count(*) or a bare identifier.
	Func *FuncCall `parser:"@@"`
	Name *string   `parser:"| @Ident"`
}

type FuncCall struct {
	Name string  `parser:"@Ident '('"`
	Star bool    `parser:"( @'*'"`
	Arg  *string `parser:"| @Ident )? ')'"`
}

type FromClause struct {
	Source string `parser:"'FROM' @('METRICS' | 'LOGS' | 'TRACES' | 'metrics' | 'logs' | 'traces')"`
}

type WhereClause struct {
	Expr *Expr `parser:"'WHERE' @@"`
}

type GroupByClause struct {
	Columns []string `parser:"'GROUP' 'BY' @Ident ( ',' @Ident )*"`
}

type SinceClause struct {
	Duration string `parser:"'SINCE' @Duration"`
}

type LimitClause struct {
	N int `parser:"'LIMIT' @Number"`
}

// ─── Expressions ──────────────────────────────────────────────────────────

type Expr struct {
	Or *OrExpr `parser:"@@"`
}

type OrExpr struct {
	Left  *AndExpr   `parser:"@@"`
	Right []*AndExpr `parser:"( 'OR' @@ )*"`
}

type AndExpr struct {
	Left  *Cmp   `parser:"@@"`
	Right []*Cmp `parser:"( 'AND' @@ )*"`
}

type Cmp struct {
	Not       *Cmp       `parser:"'NOT' @@"`
	Group     *Expr      `parser:"| '(' @@ ')'"`
	Predicate *Predicate `parser:"| @@"`
}

type Predicate struct {
	LHS *Value `parser:"@@"`
	Op  string `parser:"@( '=' | '!' '=' | '<' '=' | '>' '=' | '<' | '>' )"`
	RHS *Value `parser:"@@"`
}

type Value struct {
	String *string  `parser:"@String"`
	Number *float64 `parser:"| @Number"`
	Ident  *string  `parser:"| @Ident"`
}
