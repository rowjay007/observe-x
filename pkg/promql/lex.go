package promql

import (
	"fmt"
	"strings"
)

type tokKind int

const (
	tkIdent tokKind = iota + 1
	tkNumber
	tkString    // double-quoted
	tkLBrace    // {
	tkRBrace    // }
	tkLParen    // (
	tkRParen    // )
	tkLBracket  // [
	tkRBracket  // ]
	tkComma     // ,
	tkEq        // =
	tkNeq       // !=
	tkRegexMat  // =~
	tkRegexNoMt // !~
	tkPlus      // +
	tkMinus     // -
	tkStar      // *
	tkSlash     // /
	tkBy        // by
	tkWithout   // without
	tkDuration  // e.g. 5m, 1h
	tkGt        // >
	tkLt        // <
	tkGte       // >=
	tkLte       // <=
	tkEqCmp     // ==
)

type token struct {
	kind tokKind
	val  string
	pos  int
}

func tokenize(src string) ([]token, error) {
	out := []token{}
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '{':
			out = append(out, token{tkLBrace, "{", i})
			i++
		case c == '}':
			out = append(out, token{tkRBrace, "}", i})
			i++
		case c == '(':
			out = append(out, token{tkLParen, "(", i})
			i++
		case c == ')':
			out = append(out, token{tkRParen, ")", i})
			i++
		case c == '[':
			out = append(out, token{tkLBracket, "[", i})
			i++
		case c == ']':
			out = append(out, token{tkRBracket, "]", i})
			i++
		case c == ',':
			out = append(out, token{tkComma, ",", i})
			i++
		case c == '"':
			s, n, err := readString(src[i:])
			if err != nil {
				return nil, fmt.Errorf("at %d: %w", i, err)
			}
			out = append(out, token{tkString, s, i})
			i += n
		case c == '!':
			if i+1 < len(src) && src[i+1] == '=' {
				out = append(out, token{tkNeq, "!=", i})
				i += 2
			} else if i+1 < len(src) && src[i+1] == '~' {
				out = append(out, token{tkRegexNoMt, "!~", i})
				i += 2
			} else {
				return nil, fmt.Errorf("at %d: bare %q", i, '!')
			}
		case c == '=':
			if i+1 < len(src) && src[i+1] == '=' {
				out = append(out, token{tkEqCmp, "==", i})
				i += 2
			} else if i+1 < len(src) && src[i+1] == '~' {
				out = append(out, token{tkRegexMat, "=~", i})
				i += 2
			} else {
				out = append(out, token{tkEq, "=", i})
				i++
			}
		case c == '>':
			if i+1 < len(src) && src[i+1] == '=' {
				out = append(out, token{tkGte, ">=", i})
				i += 2
			} else {
				out = append(out, token{tkGt, ">", i})
				i++
			}
		case c == '<':
			if i+1 < len(src) && src[i+1] == '=' {
				out = append(out, token{tkLte, "<=", i})
				i += 2
			} else {
				out = append(out, token{tkLt, "<", i})
				i++
			}
		case c == '+':
			out = append(out, token{tkPlus, "+", i})
			i++
		case c == '-':
			out = append(out, token{tkMinus, "-", i})
			i++
		case c == '*':
			out = append(out, token{tkStar, "*", i})
			i++
		case c == '/':
			out = append(out, token{tkSlash, "/", i})
			i++
		case isDigit(c) || (c == '.' && i+1 < len(src) && isDigit(src[i+1])):
			s, n := readNumber(src[i:])
			out = append(out, token{tkNumber, s, i})
			i += n
		case isIdentStart(c):
			s, n := readIdent(src[i:])
			tok := token{tkIdent, s, i}
			switch s {
			case "by":
				tok.kind = tkBy
			case "without":
				tok.kind = tkWithout
			}
			// duration: digits already consumed via readNumber path,
			// so we hit here for things like "1h" — but readNumber
			// will have already eaten "1" before the alpha unit.
			// Special-case "Nh|Nm|Ns|Nd|Nw" recognition is handled
			// downstream in parseDuration.
			out = append(out, tok)
			i += n
		default:
			return nil, fmt.Errorf("at %d: unexpected %q", i, c)
		}
	}
	// Fold consecutive (tkNumber, tkIdent) where the ident is a
	// duration unit into a tkDuration. We do this here because
	// `5m` should be a single token in the parser.
	out = foldDurations(out)
	return out, nil
}

func foldDurations(in []token) []token {
	out := make([]token, 0, len(in))
	for i := 0; i < len(in); i++ {
		t := in[i]
		if t.kind == tkNumber && i+1 < len(in) && in[i+1].kind == tkIdent && isDurationUnit(in[i+1].val) {
			out = append(out, token{tkDuration, t.val + in[i+1].val, t.pos})
			i++
			continue
		}
		out = append(out, t)
	}
	return out
}

func isDurationUnit(s string) bool {
	switch s {
	case "ms", "s", "m", "h", "d", "w", "y":
		return true
	}
	return false
}

func readString(src string) (string, int, error) {
	if len(src) < 2 || src[0] != '"' {
		return "", 0, fmt.Errorf("expected string")
	}
	var b strings.Builder
	i := 1
	for i < len(src) {
		c := src[i]
		switch c {
		case '"':
			return b.String(), i + 1, nil
		case '\\':
			if i+1 >= len(src) {
				return "", 0, fmt.Errorf("dangling escape")
			}
			switch src[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte(src[i+1])
			}
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", 0, fmt.Errorf("unterminated string")
}

func readNumber(src string) (string, int) {
	i := 0
	for i < len(src) && (isDigit(src[i]) || src[i] == '.') {
		i++
	}
	return src[:i], i
}

func readIdent(src string) (string, int) {
	i := 0
	for i < len(src) && (isIdentCont(src[i])) {
		i++
	}
	return src[:i], i
}

func isIdentStart(c byte) bool {
	// Identifier start matches a strict ASCII subset; PromQL metric
	// names per the spec are [a-zA-Z_:][a-zA-Z0-9_:]*. Non-ASCII
	// identifiers are not Prometheus-conformant; we deliberately
	// don't accept them rather than carrying a unicode dependency.
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentCont(c byte) bool {
	return isIdentStart(c) || isDigit(c) || c == ':'
}
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
