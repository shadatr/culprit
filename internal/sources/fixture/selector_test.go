package fixture

import (
	"testing"
)

func TestParseSelectorValid(t *testing.T) {
	tests := []struct {
		in        string
		name      string
		matchers  int
		canonical string
	}{
		{"up", "up", 0, "up"},
		{"  http_requests:rate1m  ", "http_requests:rate1m", 0, "http_requests:rate1m"},
		{`up{job="api"}`, "up", 1, `up{job="api"}`},
		{`up{}`, "up", 0, "up"},
		{`up { job = "api" , env != 'prod', }`, "up", 2, `up{job="api",env!="prod"}`},
		{`{__name__="x", a=~"re.*"}`, "", 2, `{__name__="x",a=~"re.*"}`},
		{`{a!~"b|c"}`, "", 1, `{a!~"b|c"}`},
		{`m{a="say \"hi\"\n"}`, "m", 1, `m{a="say \"hi\"\n"}`},
		{"m{a=`raw\\d`}", "m", 1, `m{a="raw\\d"}`},
		{`m{a="ü"}`, "m", 1, `m{a="ü"}`},
		{`m{a="ü"}`, "m", 1, `m{a="ü"}`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			sel, err := ParseSelector(tt.in)
			if err != nil {
				t.Fatalf("ParseSelector: %v", err)
			}
			if sel.Name != tt.name || len(sel.Matchers) != tt.matchers {
				t.Fatalf("got name=%q matchers=%d, want %q %d", sel.Name, len(sel.Matchers), tt.name, tt.matchers)
			}
			if got := sel.String(); got != tt.canonical {
				t.Fatalf("String() = %q, want %q", got, tt.canonical)
			}
		})
	}
}

func TestParseSelectorInvalid(t *testing.T) {
	tests := []string{
		"",
		"   ",
		"{}",
		"rate(up[5m])",
		"up[5m]",
		"up offset 5m",
		"up @ 1700000000",
		"up + 1",
		"sum(up)",
		"up > 0",
		"up or down",
		`up{job="api"} / 2`,
		`up{job=api}`,
		`up{job="api"`,
		`up{job="api}`,
		`up{"job"="api"}`,
		`up{job=="api"}`,
		`up{job~"api"}`,
		`up{job="a" job="b"}`,
		`up{1job="a"}`,
		`up{a:b="c"}`,
		`up{a=~"("}`,
		`up{a="\q"}`,
		`up{a="x` + "\n" + `"}`,
		`up{a="b"}{c="d"}`,
		"1up",
		`up{a='\"'}`,
	}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			if sel, err := ParseSelector(in); err == nil {
				t.Fatalf("ParseSelector(%q) = %+v, want error", in, sel)
			}
		})
	}
}

func TestSelectorMatches(t *testing.T) {
	labels := map[string]string{"service": "catalog", "code": "500"}
	tests := []struct {
		sel  string
		name string
		want bool
	}{
		{"m", "m", true},
		{"m", "n", false},
		{`m{service="catalog"}`, "m", true},
		{`m{service="cart"}`, "m", false},
		{`m{service!="cart"}`, "m", true},
		{`m{code=~"5.."}`, "m", true},
		{`m{code=~"5"}`, "m", false}, // anchored
		{`m{code!~"4.."}`, "m", true},
		{`m{code!~"5.*"}`, "m", false},
		{`m{missing=""}`, "m", true},
		{`m{missing!=""}`, "m", false},
		{`m{missing=~".*"}`, "m", true},
		{`{__name__="m"}`, "m", true},
		{`{__name__=~"http_.*"}`, "http_requests:rate1m", true},
		{`{__name__=~"http_.*"}`, "cpu", false},
		{`{service="catalog"}`, "anything", true},
		{`m{service="catalog",code="500"}`, "m", true},
		{`m{service="catalog",code="200"}`, "m", false},
		{`m{service=~"cat|cart"}`, "m", false},
		{`m{service=~"catalog|cart"}`, "m", true},
	}
	for _, tt := range tests {
		t.Run(tt.sel+"/"+tt.name, func(t *testing.T) {
			sel, err := ParseSelector(tt.sel)
			if err != nil {
				t.Fatal(err)
			}
			if got := sel.Matches(tt.name, labels); got != tt.want {
				t.Fatalf("Matches = %v, want %v", got, tt.want)
			}
		})
	}
}

func FuzzParseSelector(f *testing.F) {
	for _, s := range []string{
		"up", `up{job="api"}`, `{__name__="x",a=~"re"}`, `m{a!~'b',c!="d"}`,
		"rate(up[5m])", "up offset 5m", `m{a="\x41é"}`, "m{a=`b`}",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		sel, err := ParseSelector(s)
		if err != nil {
			return
		}
		canon := sel.String()
		again, err := ParseSelector(canon)
		if err != nil {
			t.Fatalf("canonical form %q of %q does not reparse: %v", canon, s, err)
		}
		if again.String() != canon {
			t.Fatalf("round trip changed %q to %q", canon, again.String())
		}
		_ = sel.Matches("m", map[string]string{"a": "b"})
	})
}
