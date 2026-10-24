package gitlog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

var _ core.ChangesSource = (*Source)(nil)

var t0 = time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)

// commit describes one commit to create in a test repository.
type commit struct {
	at    time.Time
	msg   string
	files map[string]string // path -> content; "" deletes
}

func gitRun(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// makeRepo creates a repository with the given commits.
func makeRepo(t *testing.T, commits []commit) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// Isolate from the developer's git configuration.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	gitRun(t, dir, nil, "init", "-q")
	gitRun(t, dir, nil, "config", "user.name", "Ada Lovelace")
	gitRun(t, dir, nil, "config", "user.email", "ada@example.com")
	gitRun(t, dir, nil, "config", "commit.gpgsign", "false")
	for _, c := range commits {
		for p, body := range c.files {
			full := filepath.Join(dir, p)
			if body == "" {
				gitRun(t, dir, nil, "rm", "-q", p)
				continue
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			gitRun(t, dir, nil, "add", p)
		}
		date := c.at.Format(time.RFC3339)
		gitRun(t, dir, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date},
			"commit", "-q", "--allow-empty", "-m", c.msg)
	}
	return dir
}

func TestChanges(t *testing.T) {
	catalog := makeRepo(t, []commit{
		{t0.Add(-3 * time.Hour), "too old", map[string]string{"a.go": "a\n"}},
		{t0.Add(-30 * time.Minute), "add N+1 query", map[string]string{"db/query.go": "q\nq\n", "a.go": "a\nb\n"}},
		{t0.Add(10 * time.Minute), "remove a", map[string]string{"a.go": ""}},
		{t0.Add(3 * time.Hour), "too new", map[string]string{"c.go": "c\n"}},
	})
	cart := makeRepo(t, []commit{
		{t0.Add(-10 * time.Minute), "tune cache", map[string]string{"cache.go": "x\n"}},
	})
	src := New([]Repo{{Path: catalog, Service: "catalog"}, {Path: cart, Service: "cart"}})
	w := core.Window{Start: t0.Add(-time.Hour), End: t0.Add(time.Hour)}
	got, err := src.Changes(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	var summaries []string
	for _, c := range got {
		summaries = append(summaries, c.Summary)
		if c.Kind != "commit" || c.Author != "Ada Lovelace" {
			t.Errorf("change %+v: bad kind/author", c)
		}
		if !strings.HasPrefix(c.ID, c.Service+"@") || len(c.ID) < len(c.Service)+8 {
			t.Errorf("ID = %q", c.ID)
		}
		if strings.Contains(c.Author, "@") {
			t.Errorf("author leaks email: %q", c.Author)
		}
	}
	if want := []string{"remove a", "tune cache", "add N+1 query"}; !reflect.DeepEqual(summaries, want) {
		t.Fatalf("summaries = %v, want %v", summaries, want)
	}
	if !got[0].T.Equal(t0.Add(10 * time.Minute)) {
		t.Errorf("T = %v", got[0].T)
	}
	n1 := got[2]
	for _, want := range []string{"2 files changed", "3 insertions(+)", "+2 -0 db/query.go", "+1 -0 a.go"} {
		if !strings.Contains(n1.Diff, want) {
			t.Errorf("diff %q missing %q", n1.Diff, want)
		}
	}
	if !strings.Contains(got[0].Diff, "+0 -2 a.go") {
		t.Errorf("delete diff = %q", got[0].Diff)
	}
}

func TestChangesDiffTruncated(t *testing.T) {
	files := map[string]string{}
	for i := range 200 {
		files[filepath.Join("pkg", strings.Repeat("x", 20)+string(rune('a'+i%26))+strings.Repeat("y", i%7)+"_"+time.Duration(i).String()+".go")] = "x\n"
	}
	repo := makeRepo(t, []commit{{t0, "big", files}})
	got, err := New([]Repo{{Path: repo, Service: "svc"}}).Changes(context.Background(), core.Window{Start: t0.Add(-time.Minute), End: t0.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d changes", len(got))
	}
	if len(got[0].Diff) > MaxDiff || !strings.HasSuffix(got[0].Diff, "...") {
		t.Fatalf("diff len %d, suffix %q", len(got[0].Diff), got[0].Diff[len(got[0].Diff)-10:])
	}
}

func TestChangesMaxCommits(t *testing.T) {
	var cs []commit
	for i := range MaxCommits + 5 {
		cs = append(cs, commit{at: t0.Add(time.Duration(i) * time.Second), msg: "c"})
	}
	repo := makeRepo(t, cs)
	got, err := New([]Repo{{Path: repo, Service: "svc"}}).Changes(context.Background(), core.Window{Start: t0.Add(-time.Hour), End: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxCommits {
		t.Fatalf("got %d, want %d", len(got), MaxCommits)
	}
	if !got[0].T.Equal(t0.Add(time.Duration(MaxCommits+4) * time.Second)) {
		t.Fatalf("newest = %v", got[0].T)
	}
}

func TestChangesBadRepo(t *testing.T) {
	good := makeRepo(t, []commit{{t0, "ok", nil}})
	missing := filepath.Join(t.TempDir(), "nope")
	w := core.Window{Start: t0.Add(-time.Hour), End: t0.Add(time.Hour)}

	got, err := New([]Repo{{Path: missing, Service: "x"}, {Path: good, Service: "y"}}).Changes(context.Background(), w)
	if err != nil || len(got) != 1 {
		t.Fatalf("one bad repo: got %v, %v", got, err)
	}
	if _, err := New([]Repo{{Path: missing, Service: "x"}}).Changes(context.Background(), w); err == nil {
		t.Fatal("all repos bad: want error")
	}
	if got, err := New(nil).Changes(context.Background(), w); err != nil || len(got) != 0 {
		t.Fatalf("no repos: got %v, %v", got, err)
	}
}

func TestFormatStats(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"\n1\t2\ta.go\n-\t-\timg.png\n 2 files changed, 1 insertion(+), 2 deletions(-)\n",
			"2 files changed, 1 insertion(+), 2 deletions(-)\n+1 -2 a.go\nbinary img.png"},
	}
	for _, tt := range tests {
		if got := formatStats(tt.in); got != tt.want {
			t.Errorf("formatStats(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
