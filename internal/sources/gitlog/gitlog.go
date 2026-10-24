// Package gitlog lists commits from local git repositories as changes.
package gitlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

const (
	// MaxCommits caps how many commits are read per repository.
	MaxCommits = 50
	// MaxDiff caps the length of Change.Diff.
	MaxDiff = 2000

	recSep   = "\x1e"
	fieldSep = "\x1f"
)

// logFormat emits, per commit: RS full-hash US short-hash US author US
// committer-date US subject US, followed by the --numstat and --shortstat
// lines.
const logFormat = "format:" + "%x1e%H%x1f%h%x1f%an%x1f%cI%x1f%s%x1f"

// Repo is a local repository and the service it deploys.
type Repo struct {
	Path    string `yaml:"path" json:"path"`
	Service string `yaml:"service" json:"service"`
}

// Source implements core.ChangesSource.
type Source struct{ repos []Repo }

// New returns a source over repos.
func New(repos []Repo) *Source { return &Source{repos: repos} }

// Name implements core.ChangesSource.
func (s *Source) Name() string { return "git" }

// Changes returns commits in w (by committer date), newest first across all
// repositories, at most MaxCommits per repository. Change.ID is
// "<service>@<shortsha>" and Diff holds the shortstat plus per-file line
// counts, truncated to MaxDiff bytes. A repository that cannot be read is
// logged and skipped; an error is returned only if every repository fails.
func (s *Source) Changes(ctx context.Context, w core.Window) ([]core.Change, error) {
	var (
		out  []core.Change
		errs []error
	)
	for _, r := range s.repos {
		cs, err := repoChanges(ctx, r, w)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("gitlog: %w", ctx.Err())
			}
			slog.Warn("gitlog: skipping repository", "path", r.Path, "err", err)
			errs = append(errs, err)
			continue
		}
		out = append(out, cs...)
	}
	if len(s.repos) > 0 && len(errs) == len(s.repos) {
		return nil, fmt.Errorf("gitlog: %w", errors.Join(errs...))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.After(out[j].T) })
	return out, nil
}

func repoChanges(ctx context.Context, r Repo, w core.Window) ([]core.Change, error) {
	args := []string{
		"-C", r.Path,
		"-c", "core.quotepath=off",
		"-c", "log.showSignature=false",
		"log",
		"--no-color",
		fmt.Sprintf("--max-count=%d", MaxCommits),
		"--date=iso-strict",
		"--pretty=" + logFormat,
		"--numstat",
		"--shortstat",
	}
	if !w.Start.IsZero() {
		args = append(args, "--since="+w.Start.UTC().Format(time.RFC3339))
	}
	if !w.End.IsZero() {
		args = append(args, "--until="+w.End.UTC().Format(time.RFC3339))
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git log in %s: %w: %s", r.Path, err, strings.TrimSpace(stderr.String()))
	}
	return parseLog(stdout.String(), r.Service, w)
}

// parseLog turns git log output in logFormat into changes.
func parseLog(s, service string, w core.Window) ([]core.Change, error) {
	var out []core.Change
	for _, rec := range strings.Split(s, recSep) {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		f := strings.SplitN(rec, fieldSep, 6)
		if len(f) != 6 {
			return nil, fmt.Errorf("unexpected git log record %q", truncate(rec, 80))
		}
		t, err := time.Parse(time.RFC3339, f[3])
		if err != nil {
			return nil, fmt.Errorf("commit %s date: %w", f[1], err)
		}
		t = t.UTC()
		// --since/--until are inclusive in git but compare at second
		// granularity; re-check against the exact window.
		if (!w.Start.IsZero() || !w.End.IsZero()) && !inWindow(w, t) {
			continue
		}
		out = append(out, core.Change{
			ID:      service + "@" + f[1],
			Kind:    "commit",
			Service: service,
			T:       t,
			Author:  f[2],
			Summary: f[4],
			Diff:    formatStats(f[5]),
		})
	}
	return out, nil
}

func inWindow(w core.Window, t time.Time) bool {
	return (w.Start.IsZero() || !t.Before(w.Start)) && (w.End.IsZero() || !t.After(w.End))
}

// formatStats renders the --numstat/--shortstat block as the shortstat
// summary followed by one "+added -deleted path" line per file.
func formatStats(block string) string {
	var summary string
	var files []string
	for _, line := range strings.Split(block, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) == 3 && parts[0] == "-" {
			files = append(files, "binary "+parts[2])
			continue
		}
		if len(parts) == 3 {
			files = append(files, fmt.Sprintf("+%s -%s %s", parts[0], parts[1], parts[2]))
			continue
		}
		summary = strings.TrimSpace(line)
	}
	if summary == "" && len(files) == 0 {
		return ""
	}
	diff := summary
	if len(files) > 0 {
		if diff != "" {
			diff += "\n"
		}
		diff += strings.Join(files, "\n")
	}
	return truncate(diff, MaxDiff)
}

// truncate shortens s to at most n bytes without splitting a UTF-8 rune,
// marking the cut with "...".
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "\n..."
	cut := n - len(mark)
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + mark
}
