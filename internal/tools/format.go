package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/analysis/correlate"
	"github.com/shadatr/culprit/internal/analysis/logpattern"
	"github.com/shadatr/culprit/internal/core"
)

// capText trims s to at most n bytes on a line or rune boundary and marks
// the cut.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const marker = "\n...(truncated)"
	cut := n - len(marker)
	if cut < 0 {
		cut = 0
	}
	if i := strings.LastIndexByte(s[:cut], '\n'); i > cut/2 {
		cut = i
	}
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// clip shortens a single-line string to n runes.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// fmtLabels renders labels as {a="b",c="d"} with sorted keys.
func fmtLabels(l map[string]string) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", k, l[k])
	}
	b.WriteByte('}')
	return b.String()
}

// num formats a float compactly with about four significant digits.
func num(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 0):
		return "inf"
	}
	return strconv.FormatFloat(v, 'g', 4, 64)
}

// pct formats a percentage change.
func pct(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%+.0f%%", v)
}

// ts formats t as RFC3339 UTC with its offset from the incident start.
func ts(t, start time.Time) string {
	if t.IsZero() {
		return "?"
	}
	s := t.UTC().Format(time.RFC3339)
	if start.IsZero() {
		return s
	}
	d := t.Sub(start).Round(time.Second)
	sign := "+"
	if d < 0 {
		sign = "-"
		d = -d
	}
	return s + " (" + sign + d.String() + ")"
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}

// trend summarises points as n bucket averages, oldest first.
func trend(ps []core.Point, n int) string {
	if len(ps) == 0 {
		return ""
	}
	if len(ps) < n {
		n = len(ps)
	}
	parts := make([]string, 0, n)
	for i := range n {
		lo, hi := i*len(ps)/n, (i+1)*len(ps)/n
		var sum float64
		for _, p := range ps[lo:hi] {
			sum += p.V
		}
		parts = append(parts, num(sum/float64(hi-lo)))
	}
	return strings.Join(parts, " ")
}

// splitMeans returns the mean before and from start, and the max from start.
func splitMeans(ps []core.Point, start time.Time) (before, after, afterMax float64) {
	var nb, na int
	afterMax = math.Inf(-1)
	for _, p := range ps {
		if p.T.Before(start) {
			before += p.V
			nb++
			continue
		}
		after += p.V
		na++
		afterMax = max(afterMax, p.V)
	}
	if nb > 0 {
		before /= float64(nb)
	}
	if na > 0 {
		after /= float64(na)
	} else {
		afterMax = 0
	}
	return before, after, afterMax
}

var (
	reUUID = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	reHex  = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b|\b[0-9a-fA-F]{12,}\b`)
	reNum  = regexp.MustCompile(`\b\d+(\.\d+)?(ms|s|m|h|%|b|kb|mb|gb)?\b`)
	reIP   = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}(:\d+)?\b`)
)

// normalise masks variable tokens (IDs, addresses, numbers) in a message.
func normalise(msg string) string {
	p := reUUID.ReplaceAllString(msg, "<*>")
	p = reIP.ReplaceAllString(p, "<*>")
	p = reHex.ReplaceAllString(p, "<*>")
	return reNum.ReplaceAllString(p, "<*>")
}

// fallbackMine groups lines by a crude normalised message. It is used only
// when logpattern.Mine returns nothing for a non-empty input.
func fallbackMine(lines []core.LogLine) []logpattern.Template {
	idx := map[string]int{}
	var out []logpattern.Template
	levels := map[string]map[string]int{}
	for _, l := range lines {
		p := normalise(l.Message)
		i, ok := idx[p]
		if !ok {
			i = len(out)
			idx[p] = i
			out = append(out, logpattern.Template{ID: strconv.Itoa(i), Pattern: p, Service: l.Service, Sample: l.Message, First: l.T, Last: l.T})
			levels[p] = map[string]int{}
		}
		t := &out[i]
		t.Count++
		if l.T.Before(t.First) {
			t.First = l.T
		}
		if l.T.After(t.Last) {
			t.Last = l.T
		}
		levels[p][l.Level]++
	}
	for i := range out {
		best := 0
		for lv, c := range levels[out[i].Pattern] {
			if c > best || (c == best && lv < out[i].Level) {
				best, out[i].Level = c, lv
			}
		}
	}
	slices.SortStableFunc(out, func(a, b logpattern.Template) int { return b.Count - a.Count })
	return out
}

// fallbackRank orders changes by proximity to the incident start, before
// the start first. It is used only when correlate.RankChanges returns
// nothing for a non-empty input.
func fallbackRank(cs []core.Change, start time.Time) []correlate.ScoredChange {
	out := make([]correlate.ScoredChange, 0, len(cs))
	for _, c := range cs {
		out = append(out, correlate.ScoredChange{Change: c, Reasons: []string{"unscored: ordered by proximity to incident start"}})
	}
	dist := func(c core.Change) time.Duration {
		d := start.Sub(c.T)
		if d < 0 {
			return -d + 24*time.Hour // after-start changes rank below earlier ones
		}
		return d
	}
	slices.SortStableFunc(out, func(a, b correlate.ScoredChange) int {
		return int(dist(a.Change)/time.Second) - int(dist(b.Change)/time.Second)
	})
	return out
}

// truncateDiff caps a change's diff for storage.
func truncateDiff(c core.Change, n int) core.Change {
	if len(c.Diff) > n {
		c.Diff = capText(c.Diff, n)
	}
	return c
}
