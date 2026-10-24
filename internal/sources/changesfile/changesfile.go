// Package changesfile reads deploys and other changes from a JSONL file,
// one core.Change per line.
package changesfile

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sort"

	"github.com/shadatr/culprit/internal/core"
)

// Source implements core.ChangesSource.
type Source struct{ path string }

// New returns a source reading path on every call.
func New(path string) *Source { return &Source{path: path} }

// Name implements core.ChangesSource.
func (s *Source) Name() string { return "changes-file" }

// Changes returns changes in w, newest first. A missing file yields no
// changes. Blank lines are ignored; malformed lines and lines without an id
// or timestamp are skipped with a warning.
func (s *Source) Changes(ctx context.Context, w core.Window) ([]core.Change, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("changesfile: %w", err)
	}
	defer f.Close()

	var out []core.Change
	r := bufio.NewReader(f)
	for n := 1; ; n++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("changesfile: %w", err)
		}
		line, rerr := r.ReadBytes('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return nil, fmt.Errorf("changesfile: read %s: %w", s.path, rerr)
		}
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var c core.Change
			switch err := json.Unmarshal(line, &c); {
			case err != nil:
				slog.Warn("changesfile: skipping malformed line", "path", s.path, "line", n, "err", err)
			case c.ID == "" || c.T.IsZero():
				slog.Warn("changesfile: skipping line without id or t", "path", s.path, "line", n)
			case w.Contains(c.T):
				out = append(out, c)
			}
		}
		if rerr != nil {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.After(out[j].T) })
	return out, nil
}
