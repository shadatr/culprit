// Package changesfile reads deploys and other changes from a JSONL file,
// one core.Change per line.
package changesfile

import (
	"context"
	"errors"

	"github.com/shadatr/culprit/internal/core"
)

// Source implements core.ChangesSource.
type Source struct{ path string }

// New returns a source reading path on every call.
func New(path string) *Source { return &Source{path: path} }

// Name implements core.ChangesSource.
func (s *Source) Name() string { return "changes-file" }

// Changes returns changes in w, newest first.
// TODO(WS-B): implement; skip malformed lines with a slog warning.
func (s *Source) Changes(ctx context.Context, w core.Window) ([]core.Change, error) {
	return nil, errors.New("changesfile: not implemented")
}
