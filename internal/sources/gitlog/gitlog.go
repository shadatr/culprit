// Package gitlog lists commits from local git repositories as changes.
package gitlog

import (
	"context"
	"errors"

	"github.com/shadatr/culprit/internal/core"
)

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

// Changes returns commits in w, newest first, with a truncated diff stat.
// TODO(WS-B): implement with `git log` via os/exec (no shell).
func (s *Source) Changes(ctx context.Context, w core.Window) ([]core.Change, error) {
	return nil, errors.New("gitlog: not implemented")
}
