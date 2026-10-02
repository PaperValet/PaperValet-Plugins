package main

import (
	"strings"
	"testing"
)

func en(_, e string) string { return e }

func TestExtractTarget(t *testing.T) {
	cases := map[string]string{
		"8.8.8.8":                       "8.8.8.8",
		"  [2001:4860:4860::8888] ":     "2001:4860:4860::8888",
		"server at 1.2.3.4:443 is down": "1.2.3.4",
		"v6 is 2001:db8::1 ok":          "2001:db8::1",
		"see https://Example.COM/a?b=1": "example.com",
		"ping GitHub.com please":        "github.com",
		"no target here":                "",
		"999.1.1.1":                     "",
		"time 12:30:45":                 "",
	}
	for in, want := range cases {
		if got := extractTarget(in); got != want {
			t.Errorf("extractTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRender(t *testing.T) {
	out := render(en, "github.com", &ipInfo{Query: "140.82.112.3", Country: "US", Region: "California", City: "California",
		ISP: "GitHub", Org: "GitHub", AS: "AS36459 GitHub, Inc.", Hosting: true, Reverse: "lb.github.com"})
	for _, want := range []string{"140.82.112.3", "github.com", "US · California\n", "bgp.he.net/AS36459", "datacenter", "rDNS"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "Org") {
		t.Error("org equal to ISP should be hidden")
	}
}

func TestFailReason(t *testing.T) {
	if got := failReason(en, &apiError{"private range"}); !strings.Contains(got, "private") {
		t.Fatal(got)
	}
}
