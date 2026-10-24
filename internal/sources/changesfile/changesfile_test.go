package changesfile

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

var _ core.ChangesSource = (*Source)(nil)

func TestChanges(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	w := core.Window{Start: t0.Add(-time.Hour), End: t0.Add(time.Hour)}
	body := strings.Join([]string{
		`{"id":"d1","kind":"deploy","service":"catalog","t":"2026-10-20T13:30:00Z","summary":"catalog 1.8.2"}`,
		`not json`,
		``,
		`{"id":"d2","kind":"config","service":"cart","t":"2026-10-20T14:30:00Z","summary":"ttl"}`,
		`{"id":"old","kind":"deploy","t":"2026-10-20T10:00:00Z","summary":"outside"}`,
		`{"kind":"deploy","t":"2026-10-20T14:00:00Z","summary":"no id"}`,
		`{"id":"edge","kind":"flag","t":"2026-10-20T15:00:00Z","summary":"at window end"}`,
		`{"id":"last","kind":"infra","t":"2026-10-20T16:10:00+02:00","summary":"tz offset, no newline"}`,
	}, "\n")

	tests := []struct {
		name string
		body *string
		want []string
	}{
		{"filters and sorts", &body, []string{"edge", "d2", "last", "d1"}},
		{"missing file", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "changes.jsonl")
			if tt.body != nil {
				if err := os.WriteFile(path, []byte(*tt.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := New(path).Changes(context.Background(), w)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, c := range got {
				ids = append(ids, c.ID)
			}
			if !reflect.DeepEqual(ids, tt.want) {
				t.Fatalf("ids = %v, want %v", ids, tt.want)
			}
		})
	}
}

func TestChangesUnreadable(t *testing.T) {
	// A directory cannot be read as a file.
	if _, err := New(t.TempDir()).Changes(context.Background(), core.Window{}); err == nil {
		t.Fatal("want error")
	}
}
