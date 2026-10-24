package fixture

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MatchOp is a label matcher operator.
type MatchOp string

// Matcher operators, as in Prometheus.
const (
	MatchEqual     MatchOp = "="
	MatchNotEqual  MatchOp = "!="
	MatchRegexp    MatchOp = "=~"
	MatchNotRegexp MatchOp = "!~"
)

// Matcher is one label matcher such as job=~"api.*".
type Matcher struct {
	Label string
	Op    MatchOp
	Value string
	re    *regexp.Regexp // anchored; set for =~ and !~
}

// Matches reports whether v satisfies the matcher. A missing label is the
// empty string, as in Prometheus.
func (m Matcher) Matches(v string) bool {
	switch m.Op {
	case MatchEqual:
		return v == m.Value
	case MatchNotEqual:
		return v != m.Value
	case MatchRegexp:
		return m.re.MatchString(v)
	case MatchNotRegexp:
		return !m.re.MatchString(v)
	}
	return false
}

// String renders the matcher in selector syntax.
func (m Matcher) String() string {
	return m.Label + string(m.Op) + strconv.Quote(m.Value)
}

// Selector is a parsed Prometheus series selector: an optional metric name
// plus label matchers. It is the only query form Culprit's metric tools
// accept (PLAN.md §3).
type Selector struct {
	Name     string    // metric name; empty when given only via __name__ matchers
	Matchers []Matcher // label matchers in source order, excluding Name
}

// Matches reports whether a series with the given name and labels is selected.
func (s Selector) Matches(name string, labels map[string]string) bool {
	if s.Name != "" && s.Name != name {
		return false
	}
	for _, m := range s.Matchers {
		var v string
		if m.Label == "__name__" {
			v = name
		} else {
			v = labels[m.Label]
		}
		if !m.Matches(v) {
			return false
		}
	}
	return true
}

// String renders the selector in canonical form; ParseSelector(s.String())
// yields an equivalent selector.
func (s Selector) String() string {
	if len(s.Matchers) == 0 {
		return s.Name
	}
	parts := make([]string, len(s.Matchers))
	for i, m := range s.Matchers {
		parts[i] = m.String()
	}
	return s.Name + "{" + strings.Join(parts, ",") + "}"
}

// ParseSelector parses a plain Prometheus series selector such as
// `http_requests:rate1m{service="cart",code=~"5.."}`. It rejects anything
// else: functions, operators, range selectors ([5m]), offsets and @ modifiers.
// A selector must name a metric or contain at least one matcher.
func ParseSelector(s string) (Selector, error) {
	p := &selParser{src: s}
	sel, err := p.parse()
	if err != nil {
		return Selector{}, fmt.Errorf("invalid selector %q: %w", s, err)
	}
	return sel, nil
}

type selParser struct {
	src string
	pos int
}

func (p *selParser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *selParser) peek() byte {
	if p.pos < len(p.src) {
		return p.src[p.pos]
	}
	return 0
}

func (p *selParser) parse() (Selector, error) {
	var sel Selector
	p.skipSpace()
	if isNameStart(p.peek(), true) {
		sel.Name = p.ident(true)
		p.skipSpace()
	}
	if p.peek() == '{' {
		p.pos++
		ms, err := p.matchers()
		if err != nil {
			return Selector{}, err
		}
		sel.Matchers = ms
		p.skipSpace()
	}
	if p.pos < len(p.src) {
		return Selector{}, fmt.Errorf("unexpected %q at offset %d (only plain selectors are allowed, no functions, operators, ranges or offsets)", p.src[p.pos:], p.pos)
	}
	if sel.Name == "" && len(sel.Matchers) == 0 {
		return Selector{}, fmt.Errorf("empty selector")
	}
	return sel, nil
}

func (p *selParser) matchers() ([]Matcher, error) {
	var out []Matcher
	for {
		p.skipSpace()
		if p.peek() == '}' {
			p.pos++
			return out, nil
		}
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("unterminated matcher list")
		}
		if !isNameStart(p.peek(), false) {
			return nil, fmt.Errorf("expected label name at offset %d", p.pos)
		}
		label := p.ident(false)
		p.skipSpace()
		op, err := p.op()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		val, err := p.str()
		if err != nil {
			return nil, err
		}
		m := Matcher{Label: label, Op: op, Value: val}
		if op == MatchRegexp || op == MatchNotRegexp {
			re, err := regexp.Compile("^(?:" + val + ")$")
			if err != nil {
				return nil, fmt.Errorf("bad regex for %s: %w", label, err)
			}
			m.re = re
		}
		out = append(out, m)
		p.skipSpace()
		switch p.peek() {
		case ',':
			p.pos++
		case '}':
		default:
			return nil, fmt.Errorf("expected ',' or '}' at offset %d", p.pos)
		}
	}
}

func (p *selParser) op() (MatchOp, error) {
	rest := p.src[p.pos:]
	for _, op := range []MatchOp{MatchRegexp, MatchNotRegexp, MatchNotEqual, MatchEqual} {
		if strings.HasPrefix(rest, string(op)) {
			p.pos += len(op)
			return op, nil
		}
	}
	return "", fmt.Errorf("expected matcher operator at offset %d", p.pos)
}

// str parses a double-quoted, single-quoted or backtick string literal.
func (p *selParser) str() (string, error) {
	q := p.peek()
	if q != '"' && q != '\'' && q != '`' {
		return "", fmt.Errorf("expected quoted string at offset %d", p.pos)
	}
	start := p.pos
	p.pos++
	if q == '`' {
		end := strings.IndexByte(p.src[p.pos:], '`')
		if end < 0 {
			return "", fmt.Errorf("unterminated string at offset %d", start)
		}
		v := p.src[p.pos : p.pos+end]
		p.pos += end + 1
		if !utf8.ValidString(v) {
			return "", fmt.Errorf("invalid UTF-8 in string at offset %d", start)
		}
		return v, nil
	}
	var b strings.Builder
	for {
		if p.pos >= len(p.src) {
			return "", fmt.Errorf("unterminated string at offset %d", start)
		}
		c := p.src[p.pos]
		switch {
		case c == q:
			p.pos++
			return b.String(), nil
		case c == '\n':
			return "", fmt.Errorf("newline in string at offset %d", p.pos)
		case c == '\\':
			// Reuse Go's escape rules, which Prometheus also follows.
			r, mb, tail, err := strconv.UnquoteChar(p.src[p.pos:], q)
			if err != nil {
				return "", fmt.Errorf("bad escape at offset %d", p.pos)
			}
			if mb || r >= utf8.RuneSelf {
				b.WriteRune(r)
			} else {
				b.WriteByte(byte(r))
			}
			p.pos = len(p.src) - len(tail)
		default:
			r, size := utf8.DecodeRuneInString(p.src[p.pos:])
			if r == utf8.RuneError && size == 1 {
				return "", fmt.Errorf("invalid UTF-8 at offset %d", p.pos)
			}
			b.WriteString(p.src[p.pos : p.pos+size])
			p.pos += size
		}
	}
}

func (p *selParser) ident(allowColon bool) string {
	start := p.pos
	for p.pos < len(p.src) && isNameChar(p.src[p.pos], allowColon) {
		p.pos++
	}
	return p.src[start:p.pos]
}

func isNameStart(c byte, allowColon bool) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (allowColon && c == ':')
}

func isNameChar(c byte, allowColon bool) bool {
	return isNameStart(c, allowColon) || (c >= '0' && c <= '9')
}
