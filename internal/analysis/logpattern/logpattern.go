// Package logpattern groups log lines into templates and compares template
// frequencies before and during an incident.
//
// Mining is Drain-style:
//
//  1. Each message is pre-masked: quoted strings, numbers (with units such as
//     250ms, 1.5GiB, 30%), durations, dates and times, UUIDs, hex ids, IPs
//     (with ports), emails, numeric/id segments of URL and file paths, values
//     of key=value pairs that look variable, and runs of 4+ digits inside
//     identifiers all become <*>.
//  2. The masked tokens (split on whitespace) are routed through a fixed-depth
//     prefix tree keyed by service, token count and first token.
//  3. In the leaf, the line joins the most similar template if at least half of
//     the positions (and at least two tokens) are equal; positions that differ
//     become <*>. Otherwise it starts a new template.
//
// Lines are processed in time order (stable), so results are deterministic.
package logpattern

import (
	"fmt"
	"hash/fnv"
	"math"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

// Wildcard replaces variable parts of a message.
const Wildcard = "<*>"

const (
	simThreshold = 0.5
	minRatio     = 2.0 // smoothed incident/baseline rate ratio a rise must reach
)

// Template is a group of similar log lines, e.g.
// "timeout after <*> ms calling <*>".
type Template struct {
	ID      string    `json:"id"` // stable hash of Service and Pattern
	Pattern string    `json:"pattern"`
	Count   int       `json:"count"`
	Level   string    `json:"level,omitempty"` // most common level
	Service string    `json:"service,omitempty"`
	Sample  string    `json:"sample"`
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
}

// Shift is a template whose rate changed between baseline and incident.
type Shift struct {
	Template        Template  `json:"template"`
	BaselineRate    float64   `json:"baseline_rate"` // lines per minute before incident start
	IncidentRate    float64   `json:"incident_rate"` // lines per minute from incident start
	New             bool      `json:"new"`           // never seen in baseline
	Score           float64   `json:"score"`
	FirstInIncident time.Time `json:"first_in_incident"`
}

// Mine groups lines into templates sorted by Count, highest first, then by
// Pattern and Service. Templates are per service.
func Mine(lines []core.LogLine) []Template {
	clusters, _ := mine(lines)
	out := make([]Template, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, c.template(lines))
	}
	sortTemplates(out)
	return out
}

// Diff mines all lines together and returns templates whose rate rose (or
// that are new) at/after incidentStart, sorted by Score, highest first.
//
// Rates are lines per minute over the window the lines actually cover: from
// the earliest line to incidentStart for the baseline, and from incidentStart
// to the latest line for the incident (each at least one minute). If there are
// no baseline lines at all nothing can be called a rise and Diff returns nil.
//
// Score = levelWeight * log2(ratio) * (1 + ln(1 + incidentRate - baselineRate))
// where ratio = ((incident+1)/incidentMinutes) / ((baseline+1)/baselineMinutes)
// is add-one smoothed, and levelWeight is fatal 4, error 3, warn 2, info 1,
// debug 0.5. A template is reported when the smoothed ratio is at least
// 2 (a 50% wiggle in a steady template is noise), and it had at least 2 incident lines (1 is enough for a new error/fatal
// template).
func Diff(lines []core.LogLine, incidentStart time.Time) []Shift {
	if len(lines) == 0 {
		return nil
	}
	clusters, order := mine(lines)
	minT, maxT := lines[order[0]].T, lines[order[len(order)-1]].T
	if !minT.Before(incidentStart) {
		return nil
	}
	baseMin := math.Max(1, incidentStart.Sub(minT).Minutes())
	incMin := math.Max(1, maxT.Sub(incidentStart).Minutes())

	var out []Shift
	for _, c := range clusters {
		var nb, ni int
		var first time.Time
		for _, idx := range c.members {
			t := lines[idx].T
			if t.Before(incidentStart) {
				nb++
				continue
			}
			if ni == 0 {
				first = t
			}
			ni++
		}
		if ni == 0 {
			continue
		}
		tpl := c.template(lines)
		w := levelWeight(tpl.Level)
		isNew := nb == 0
		if ni < 2 && !(isNew && w >= 3) {
			continue
		}
		br, ir := float64(nb)/baseMin, float64(ni)/incMin
		ratio := (float64(ni+1) / incMin) / (float64(nb+1) / baseMin)
		if ratio < minRatio || ir <= br {
			continue
		}
		score := w * math.Log2(ratio) * (1 + math.Log1p(ir-br))
		out = append(out, Shift{
			Template:        tpl,
			BaselineRate:    round(br),
			IncidentRate:    round(ir),
			New:             isNew,
			Score:           round(score),
			FirstInIncident: first,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Template.Pattern != out[j].Template.Pattern {
			return out[i].Template.Pattern < out[j].Template.Pattern
		}
		return out[i].Template.Service < out[j].Template.Service
	})
	return out
}

// LevelWeight returns the severity weight used by Diff: fatal 4, error 3,
// warn 2, debug/trace 0.5, anything else 1.
func LevelWeight(level string) float64 { return levelWeight(level) }

func levelWeight(level string) float64 {
	switch normLevel(level) {
	case "fatal":
		return 4
	case "error":
		return 3
	case "warn":
		return 2
	case "debug":
		return 0.5
	}
	return 1
}

func normLevel(level string) string {
	l := strings.ToLower(strings.TrimSpace(level))
	switch l {
	case "fatal", "panic", "crit", "critical", "emerg", "emergency", "alert":
		return "fatal"
	case "error", "err", "eror":
		return "error"
	case "warn", "warning":
		return "warn"
	case "debug", "trace", "dbug":
		return "debug"
	case "info", "information", "notice":
		return "info"
	}
	return l
}

// cluster is one template being built.
type cluster struct {
	service string
	tokens  []string
	members []int // indices into lines, in processing order
}

func (c *cluster) template(lines []core.LogLine) Template {
	pattern := strings.Join(c.tokens, " ")
	first, last := lines[c.members[0]], lines[c.members[len(c.members)-1]]
	t := Template{
		ID:      templateID(c.service, pattern),
		Pattern: pattern,
		Count:   len(c.members),
		Service: c.service,
		Sample:  first.Message,
		First:   first.T,
		Last:    last.T,
	}
	levels := map[string]int{}
	for _, idx := range c.members {
		levels[normLevel(lines[idx].Level)]++
	}
	best := -1
	for l, n := range levels {
		if n > best || (n == best && (levelWeight(l) > levelWeight(t.Level) || (levelWeight(l) == levelWeight(t.Level) && l < t.Level))) {
			t.Level, best = l, n
		}
	}
	return t
}

func templateID(service, pattern string) string {
	h := fnv.New64a()
	h.Write([]byte(service))
	h.Write([]byte{0})
	h.Write([]byte(pattern))
	return fmt.Sprintf("%012x", h.Sum64()&0xffffffffffff)
}

// mine returns the clusters (deduplicated by service and pattern, in creation
// order) and the time order of lines.
func mine(lines []core.LogLine) ([]*cluster, []int) {
	order := make([]int, len(lines))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return lines[order[a]].T.Before(lines[order[b]].T) })

	leaves := map[string][]*cluster{}
	var all []*cluster
	var key strings.Builder
	for _, idx := range order {
		ln := lines[idx]
		toks := Tokens(ln.Message)
		key.Reset()
		key.WriteString(ln.Service)
		key.WriteByte(0)
		key.WriteString(strconv.Itoa(len(toks)))
		key.WriteByte(0)
		if len(toks) > 0 {
			key.WriteString(firstKey(toks[0]))
		}
		k := key.String()
		leaf := leaves[k]
		var best *cluster
		bestMatch := -1
		for _, c := range leaf {
			m := matches(c.tokens, toks)
			if m > bestMatch {
				best, bestMatch = c, m
				if m == len(toks) {
					break
				}
			}
		}
		if best != nil && accept(bestMatch, len(toks)) {
			for i, tk := range toks {
				if best.tokens[i] != tk {
					best.tokens[i] = Wildcard
				}
			}
			best.members = append(best.members, idx)
			continue
		}
		c := &cluster{service: ln.Service, tokens: append([]string(nil), toks...), members: []int{idx}}
		leaves[k] = append(leaf, c)
		all = append(all, c)
	}

	// Different clusters can converge on the same pattern; merge them.
	byPattern := map[string]*cluster{}
	var out []*cluster
	for _, c := range all {
		pk := c.service + "\x00" + strings.Join(c.tokens, " ")
		if prev, ok := byPattern[pk]; ok {
			prev.members = mergeSorted(prev.members, c.members, lines)
			continue
		}
		byPattern[pk] = c
		out = append(out, c)
	}
	return out, order
}

func accept(match, n int) bool {
	if n == 0 {
		return true
	}
	if match == n {
		return true
	}
	return match >= 2 && float64(match)/float64(n) >= simThreshold
}

func matches(tpl, toks []string) int {
	m := 0
	for i, t := range tpl {
		if t == toks[i] {
			m++
		}
	}
	return m
}

func firstKey(tok string) string {
	if strings.Contains(tok, Wildcard) || strings.ContainsAny(tok, "0123456789") {
		return Wildcard
	}
	return tok
}

func mergeSorted(a, b []int, lines []core.LogLine) []int {
	out := make([]int, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ta, tb := lines[a[i]].T, lines[b[j]].T
		if tb.Before(ta) || (tb.Equal(ta) && b[j] < a[i]) {
			out = append(out, b[j])
			j++
		} else {
			out = append(out, a[i])
			i++
		}
	}
	out = append(out, a[i:]...)
	return append(out, b[j:]...)
}

func sortTemplates(t []Template) {
	sort.SliceStable(t, func(i, j int) bool {
		if t[i].Count != t[j].Count {
			return t[i].Count > t[j].Count
		}
		if t[i].Pattern != t[j].Pattern {
			return t[i].Pattern < t[j].Pattern
		}
		return t[i].Service < t[j].Service
	})
}

func round(v float64) float64 { return math.Round(v*1e4) / 1e4 }

// Tokens returns the whitespace tokens of msg with variable parts masked to
// <*>. It is exported so callers can match a message against a Pattern.
func Tokens(msg string) []string {
	toks := strings.Fields(maskQuoted(msg))
	for i, t := range toks {
		toks[i] = maskToken(t)
	}
	return toks
}

// Mask returns msg with variable parts replaced by <*> (see Tokens).
func Mask(msg string) string { return strings.Join(Tokens(msg), " ") }

// maskQuoted replaces "..." and '...' spans with <*>. A quote only opens at the
// start of the message or after whitespace or one of = : ( [ { , and a single
// quote only closes before whitespace, punctuation or the end, so apostrophes
// in words such as "can't" are left alone.
func maskQuoted(s string) string {
	if !strings.ContainsAny(s, `"'`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c == '"' || c == '\'') && (i == 0 || strings.IndexByte(" \t=:([{,", s[i-1]) >= 0) {
			if end := closingQuote(s, i+1, c); end > 0 {
				b.WriteString(Wildcard)
				i = end
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func closingQuote(s string, from int, q byte) int {
	for j := from; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case q:
			if q == '"' || j+1 == len(s) || strings.IndexByte(" \t,;:.)]}", s[j+1]) >= 0 {
				return j
			}
		}
	}
	return -1
}

const (
	leadPunct  = `([{<"'`
	trailPunct = `,;:.)]}>"'!?`
)

func maskToken(t string) string {
	if t == Wildcard || !strings.ContainsAny(t, "0123456789@:/=") {
		return t
	}
	// Peel surrounding punctuation, but never break up a wildcard.
	i := 0
	for i < len(t) && strings.IndexByte(leadPunct, t[i]) >= 0 && !strings.HasPrefix(t[i:], Wildcard) {
		i++
	}
	j := len(t)
	for j > i && strings.IndexByte(trailPunct, t[j-1]) >= 0 && !strings.HasSuffix(t[i:j], Wildcard) {
		j--
	}
	mid := t[i:j]
	if mid == "" || mid == Wildcard {
		return t
	}
	return t[:i] + maskCore(mid) + t[j:]
}

func maskCore(s string) string {
	if isVariable(s) {
		return Wildcard
	}
	if i := strings.Index(s, "://"); i > 0 {
		return maskURL(s, i)
	}
	if strings.Contains(s, "/") {
		return maskPath(s)
	}
	if k, v, ok := strings.Cut(s, "="); ok && k != "" {
		if v == "" {
			return s
		}
		return k + "=" + maskCore(v)
	}
	if k, v, ok := strings.Cut(s, ":"); ok && k != "" && v != "" {
		return maskCore(k) + ":" + maskCore(v)
	}
	return maskDigitRuns(s)
}

func maskURL(s string, schemeEnd int) string {
	rest := s[schemeEnd+3:]
	query := ""
	if q := strings.IndexAny(rest, "?#"); q >= 0 {
		query, rest = rest[q:q+1]+Wildcard, rest[:q]
	}
	host, path, _ := strings.Cut(rest, "/")
	if isVariable(host) {
		host = Wildcard
	} else if h, p, ok := strings.Cut(host, ":"); ok && isNumber(p) {
		host = h + ":" + Wildcard
	}
	out := s[:schemeEnd+3] + host
	if strings.Contains(rest, "/") {
		out += "/" + maskPath(path)
	}
	return out + query
}

func maskPath(p string) string {
	query := ""
	if q := strings.IndexByte(p, '?'); q >= 0 {
		query, p = "?"+Wildcard, p[:q]
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		if isVariable(seg) {
			segs[i] = Wildcard
		} else {
			segs[i] = maskDigitRuns(seg)
		}
	}
	return strings.Join(segs, "/") + query
}

// maskDigitRuns replaces runs of 4 or more digits inside identifiers, e.g.
// order-123456 -> order-<*>, and hex-looking parts between - _ . separators.
func maskDigitRuns(s string) string {
	if strings.ContainsAny(s, "-_.") {
		parts := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
		changed := false
		for _, p := range parts {
			if isVariable(p) || isHexID(p, 6) {
				changed = true
				break
			}
		}
		if changed {
			var b strings.Builder
			start := 0
			for i := 0; i <= len(s); i++ {
				if i == len(s) || s[i] == '-' || s[i] == '_' || s[i] == '.' {
					p := s[start:i]
					if p != "" && (isVariable(p) || isHexID(p, 6)) {
						b.WriteString(Wildcard)
					} else {
						b.WriteString(p)
					}
					if i < len(s) {
						b.WriteByte(s[i])
					}
					start = i + 1
				}
			}
			s = b.String()
		}
	}
	var b strings.Builder
	run := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] >= '0' && s[i] <= '9' {
			run++
			continue
		}
		if run > 0 {
			if run >= 4 {
				b.WriteString(Wildcard)
			} else {
				b.WriteString(s[i-run : i])
			}
			run = 0
		}
		if i < len(s) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// isVariable reports whether a whole token is a value rather than a word.
func isVariable(s string) bool {
	if s == "" {
		return false
	}
	return isNumberish(s) || isUUID(s) || isHexID(s, 8) || isIP(s) || isEmail(s) || isDuration(s) || isTimestamp(s)
}

func isTimestamp(s string) bool {
	if len(s) < 19 || s[4] != '-' || s[0] < '0' || s[0] > '9' {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// isNumberish matches numbers, dates, times and versions (digits mixed with
// . : , - + _ /), optionally followed by a unit of up to 3 letters or %.
func isNumberish(s string) bool {
	i := 0
	if s[0] == '+' || s[0] == '-' {
		i++
	}
	if i >= len(s) || s[i] < '0' || s[i] > '9' {
		return false
	}
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || strings.IndexByte(".:,-+_/", s[i]) >= 0) {
		i++
	}
	unit := s[i:]
	if unit == "" || unit == "%" || unit == "µs" {
		return true
	}
	if len(unit) > 3 {
		return false
	}
	for _, r := range unit {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

func isDuration(s string) bool {
	if s[0] < '0' || s[0] > '9' {
		return false
	}
	_, err := time.ParseDuration(s)
	return err == nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !isHex(c) {
			return false
		}
	}
	return true
}

// isHexID matches 0x-prefixed hex, or hex strings of at least minLen
// characters containing at least one digit (trace ids, commit shas).
func isHexID(s string, minLen int) bool {
	if len(s) > 2 && (s[:2] == "0x" || s[:2] == "0X") {
		for i := 2; i < len(s); i++ {
			if !isHex(s[i]) {
				return false
			}
		}
		return true
	}
	if len(s) < minLen {
		return false
	}
	digit := false
	for i := 0; i < len(s); i++ {
		if !isHex(s[i]) {
			return false
		}
		if s[i] <= '9' {
			digit = true
		}
	}
	return digit
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func isIP(s string) bool {
	if !strings.ContainsAny(s, ".:") {
		return false
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	_, err := netip.ParseAddrPort(s)
	return err == nil
}

func isEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && strings.Contains(s[at+1:], ".") && !strings.ContainsAny(s, "/")
}
