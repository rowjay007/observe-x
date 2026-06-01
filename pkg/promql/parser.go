package promql

import (
	"fmt"
	"strconv"
	"time"
)

// ── AST ──────────────────────────────────────────────────────────

type matcherOp int

const (
	matchEq matcherOp = iota
	matchNeq
	matchRegex
	matchNotRegex
)

type matcher struct {
	name string
	op   matcherOp
	val  string
}

// vectorSelector: metric_name{matchers}
type vectorSelector struct {
	metric  string
	matches []matcher
}

// rangeSelector: vectorSelector[duration]
type rangeSelector struct {
	v       vectorSelector
	dur     time.Duration
	durText string // original text, e.g. "5m" — kept for error messages
}

// callExpr: ident(args...)
type callExpr struct {
	name string
	args []node
}

// aggrExpr: <op>(arg) [by/without (labels)]
type aggrExpr struct {
	op      string // sum|avg|min|max|count|quantile
	arg     node
	by      []string
	without []string
	// optional scalar arg for quantile / topk-like ops (first arg
	// in our supported subset is the φ for quantile).
	phi *float64
}

// numLit: numeric literal
type numLit struct{ v float64 }

// binExpr: arith / cmp with scalar on one side. We only support
// `aggr OP scalar` and `aggr CMP scalar`.
type binExpr struct {
	op    string // + - * / > < >= <= == !=
	left  node
	right node
}

type node interface{ nodeMark() }

func (vectorSelector) nodeMark() {}
func (rangeSelector) nodeMark()  {}
func (callExpr) nodeMark()       {}
func (aggrExpr) nodeMark()       {}
func (numLit) nodeMark()         {}
func (binExpr) nodeMark()        {}

// ── parser ───────────────────────────────────────────────────────

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() *token {
	if p.pos >= len(p.tokens) {
		return nil
	}
	return &p.tokens[p.pos]
}
func (p *parser) eat() *token {
	t := p.peek()
	if t != nil {
		p.pos++
	}
	return t
}
func (p *parser) expect(k tokKind) (*token, error) {
	t := p.eat()
	if t == nil {
		return nil, fmt.Errorf("expected %v, got EOF", k)
	}
	if t.kind != k {
		return nil, fmt.Errorf("at %d: expected %v, got %v (%q)", t.pos, k, t.kind, t.val)
	}
	return t, nil
}

// expr := binCmp
// binCmp := binAdd ((>|<|>=|<=|==|!=) binAdd)?
// binAdd := binMul ((+|-) binMul)*
// binMul := primary ((*|/) primary)*
// primary := number | aggr | call | vector | range | "(" expr ")"
func parseExpr(p *parser) (node, error) {
	return parseBinCmp(p)
}

func parseBinCmp(p *parser) (node, error) {
	left, err := parseBinAdd(p)
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t != nil {
		switch t.kind {
		case tkGt, tkLt, tkGte, tkLte, tkEqCmp, tkNeq:
			p.eat()
			right, err := parseBinAdd(p)
			if err != nil {
				return nil, err
			}
			return binExpr{op: t.val, left: left, right: right}, nil
		}
	}
	return left, nil
}

func parseBinAdd(p *parser) (node, error) {
	left, err := parseBinMul(p)
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t == nil || (t.kind != tkPlus && t.kind != tkMinus) {
			return left, nil
		}
		p.eat()
		right, err := parseBinMul(p)
		if err != nil {
			return nil, err
		}
		left = binExpr{op: t.val, left: left, right: right}
	}
}

func parseBinMul(p *parser) (node, error) {
	left, err := parsePrimary(p)
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t == nil || (t.kind != tkStar && t.kind != tkSlash) {
			return left, nil
		}
		p.eat()
		right, err := parsePrimary(p)
		if err != nil {
			return nil, err
		}
		left = binExpr{op: t.val, left: left, right: right}
	}
}

func parsePrimary(p *parser) (node, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("unexpected EOF")
	}
	switch t.kind {
	case tkNumber:
		p.eat()
		f, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return nil, fmt.Errorf("at %d: bad number %q", t.pos, t.val)
		}
		return numLit{v: f}, nil
	case tkLParen:
		p.eat()
		inner, err := parseExpr(p)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tkRParen); err != nil {
			return nil, err
		}
		return inner, nil
	case tkIdent:
		return parseIdentLike(p)
	default:
		return nil, fmt.Errorf("at %d: unexpected token %v %q", t.pos, t.kind, t.val)
	}
}

// parseIdentLike: aggregator, call, or selector.
func parseIdentLike(p *parser) (node, error) {
	name := p.eat()
	next := p.peek()
	if next != nil && next.kind == tkLParen {
		return parseCallOrAggr(p, name.val)
	}
	// Bare metric selector — may have {} matchers and an optional
	// [duration] for range selectors.
	return parseSelectorTail(p, name.val)
}

func parseCallOrAggr(p *parser, name string) (node, error) {
	if _, err := p.expect(tkLParen); err != nil {
		return nil, err
	}
	// Collect args separated by comma.
	args := []node{}
	if t := p.peek(); t == nil || t.kind != tkRParen {
		for {
			a, err := parseExpr(p)
			if err != nil {
				return nil, err
			}
			args = append(args, a)
			t := p.peek()
			if t != nil && t.kind == tkComma {
				p.eat()
				continue
			}
			break
		}
	}
	if _, err := p.expect(tkRParen); err != nil {
		return nil, err
	}

	if isAggrName(name) {
		ae := aggrExpr{op: name}
		switch name {
		case "quantile":
			if len(args) != 2 {
				return nil, fmt.Errorf("quantile expects (phi, vector), got %d args", len(args))
			}
			lit, ok := args[0].(numLit)
			if !ok {
				return nil, fmt.Errorf("quantile phi must be a numeric literal")
			}
			ae.phi = &lit.v
			ae.arg = args[1]
		default:
			if len(args) != 1 {
				return nil, fmt.Errorf("%s expects 1 arg, got %d", name, len(args))
			}
			ae.arg = args[0]
		}
		// Optional `by (l1,l2)` / `without (l1,l2)` modifier.
		if t := p.peek(); t != nil && (t.kind == tkBy || t.kind == tkWithout) {
			modKind := t.kind
			p.eat()
			if _, err := p.expect(tkLParen); err != nil {
				return nil, err
			}
			labs := []string{}
			for {
				id, err := p.expect(tkIdent)
				if err != nil {
					return nil, err
				}
				labs = append(labs, id.val)
				if t := p.peek(); t != nil && t.kind == tkComma {
					p.eat()
					continue
				}
				break
			}
			if _, err := p.expect(tkRParen); err != nil {
				return nil, err
			}
			if modKind == tkBy {
				ae.by = labs
			} else {
				ae.without = labs
			}
		}
		return ae, nil
	}
	// Plain call (rate/irate/increase/X_over_time…).
	return callExpr{name: name, args: args}, nil
}

func isAggrName(s string) bool {
	switch s {
	case "sum", "avg", "min", "max", "count", "quantile":
		return true
	}
	return false
}

func parseSelectorTail(p *parser, metric string) (node, error) {
	vs := vectorSelector{metric: metric}
	if t := p.peek(); t != nil && t.kind == tkLBrace {
		p.eat()
		// matcher list, comma-separated
		for {
			t := p.peek()
			if t == nil {
				return nil, fmt.Errorf("unterminated label matchers")
			}
			if t.kind == tkRBrace {
				p.eat()
				break
			}
			name, err := p.expect(tkIdent)
			if err != nil {
				return nil, err
			}
			opT := p.eat()
			if opT == nil {
				return nil, fmt.Errorf("expected matcher op")
			}
			var op matcherOp
			switch opT.kind {
			case tkEq:
				op = matchEq
			case tkNeq:
				op = matchNeq
			case tkRegexMat:
				op = matchRegex
			case tkRegexNoMt:
				op = matchNotRegex
			default:
				return nil, fmt.Errorf("at %d: expected matcher op, got %q", opT.pos, opT.val)
			}
			val, err := p.expect(tkString)
			if err != nil {
				return nil, err
			}
			vs.matches = append(vs.matches, matcher{name: name.val, op: op, val: val.val})
			t = p.peek()
			if t != nil && t.kind == tkComma {
				p.eat()
				continue
			}
		}
	}
	// Optional `[duration]` → range selector.
	if t := p.peek(); t != nil && t.kind == tkLBracket {
		p.eat()
		dt := p.eat()
		if dt == nil || dt.kind != tkDuration {
			return nil, fmt.Errorf("expected duration in []")
		}
		if _, err := p.expect(tkRBracket); err != nil {
			return nil, err
		}
		dur, err := parseDuration(dt.val)
		if err != nil {
			return nil, fmt.Errorf("bad duration %q: %w", dt.val, err)
		}
		return rangeSelector{v: vs, dur: dur, durText: dt.val}, nil
	}
	return vs, nil
}

func parseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("too short")
	}
	// trailing unit
	var unit string
	if len(s) >= 2 && s[len(s)-2:] == "ms" {
		unit = "ms"
	} else {
		unit = s[len(s)-1:]
	}
	numStr := s[:len(s)-len(unit)]
	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, err
	}
	var mult time.Duration
	switch unit {
	case "ms":
		mult = time.Millisecond
	case "s":
		mult = time.Second
	case "m":
		mult = time.Minute
	case "h":
		mult = time.Hour
	case "d":
		mult = 24 * time.Hour
	case "w":
		mult = 7 * 24 * time.Hour
	case "y":
		mult = 365 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("unknown unit %q", unit)
	}
	return time.Duration(float64(mult) * n), nil
}
