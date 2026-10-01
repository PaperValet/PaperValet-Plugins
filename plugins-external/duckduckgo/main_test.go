package main

import (
	"strings"
	"testing"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func TestParseLimitAndQuery(t *testing.T) {
	cases := []struct {
		in    []string
		q     string
		limit int
	}{
		{[]string{"golang", "generics"}, "golang generics", 8},
		{[]string{"golang", "-n", "5"}, "golang", 5},
		{[]string{"-n", "99", "golang"}, "golang", 15},
		{[]string{"golang", "--limit", "0"}, "golang", 1},
		{[]string{"golang", "-n3"}, "golang", 3},
		{[]string{"golang", "-n", "abc"}, "golang -n abc", 8},
	}
	for _, c := range cases {
		q, l := parseLimitAndQuery(c.in)
		if q != c.q || l != c.limit {
			t.Errorf("%v: got %q %d", c.in, q, l)
		}
	}
}

const htmlFixture = `<div id="links" class="results">
<div class="result results_links results_links_deep web-result result--ad"><a class="result__a" href="https://duckduckgo.com/y.js?ad_provider=x">Ad</a></div>
<div class="result results_links results_links_deep web-result ">
  <h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2Ftutorial%2Fgenerics&amp;rut=abc">Tutorial: <b>generics</b> &amp; more</a></h2>
  <a class="result__url" href="https://go.dev/doc/tutorial/generics">
    go.dev/doc/tutorial/generics
  </a>
  <a class="result__snippet" href="https://go.dev/doc/tutorial/generics">Learn how to use <b>generics</b> in Go.</a>
</div>
<div class="result results_links results_links_deep web-result ">
  <h2 class="result__title"><a rel="nofollow" class="result__a" href="https://www.example.com/x">Example</a></h2>
</div>
</div>`

func TestParseHTML(t *testing.T) {
	r := parseHTMLResults(htmlFixture, 8)
	if len(r) != 2 {
		t.Fatalf("%+v", r)
	}
	if r[0].URL != "https://go.dev/doc/tutorial/generics" || r[0].Title != "Tutorial: generics & more" ||
		r[0].Snippet != "Learn how to use generics in Go." || r[0].Display != "go.dev/doc/tutorial/generics" {
		t.Fatalf("%+v", r[0])
	}
	if r[1].Display != "example.com" {
		t.Fatalf("%+v", r[1])
	}
	if len(parseHTMLResults(htmlFixture, 1)) != 1 {
		t.Fatal("limit")
	}
	if parseHTMLResults(`<div class="anomaly-modal">`, 5) != nil {
		t.Fatal("anomaly")
	}
}

const liteFixture = `<table border="0">
<tr class="result-sponsored"><td>1.</td><td><a rel="nofollow" href="https://ads.example/" class='result-link'>Ad</a></td></tr>
<tr><td valign="top">1.&nbsp;</td><td>
<a rel="nofollow" href="https://go.dev/doc/tutorial/generics" class='result-link'>Tutorial: Getting started with generics</a>
</td></tr>
<tr><td>&nbsp;</td><td class='result-snippet'>
  Learn how to use <b>generics</b> in Go.
</td></tr>
<tr><td>&nbsp;</td><td><span class='link-text'>go.dev/doc/tutorial/generics</span></td></tr>
<tr><td valign="top">2.&nbsp;</td><td>
<a rel="nofollow" href="https://go.dev/blog/intro-generics" class='result-link'>An Introduction To Generics</a>
</td></tr>
</table>`

func TestParseLite(t *testing.T) {
	r := parseLiteResults(liteFixture, 8)
	if len(r) != 2 {
		t.Fatalf("%+v", r)
	}
	if r[0].Snippet != "Learn how to use generics in Go." || r[0].Display != "go.dev/doc/tutorial/generics" {
		t.Fatalf("%+v", r[0])
	}
	if r[1].Snippet != "" || r[1].Display != "go.dev" {
		t.Fatalf("%+v", r[1])
	}
}

func TestFirecrawlParse(t *testing.T) {
	v2 := `{"success":true,"data":{"web":[{"url":"https://go.dev/","title":"Go","description":"# Build ¶\n- simple ![x](y) [link](z)<br>&amp;"}]}}`
	items, err := parseFirecrawl([]byte(v2))
	if err != nil || len(items) != 1 || items[0].URL != "https://go.dev/" {
		t.Fatal(items, err)
	}
	if c := cleanMarkdown(items[0].Description); c != "Build simple link &" {
		t.Fatalf("%q", c)
	}
	v1 := `{"success":true,"data":[{"url":"https://a.com","title":"A"}]}`
	if items, _ := parseFirecrawl([]byte(v1)); len(items) != 1 {
		t.Fatal("v1")
	}
}

func TestDedupe(t *testing.T) {
	in := []result{{Title: "a", URL: "https://go.dev/"}, {Title: "b", URL: "http://www.go.dev"}, {Title: "c", URL: "https://x.dev"}}
	if out := dedupe(in, 10); len(out) != 2 {
		t.Fatal(out)
	}
	if out := dedupe(in, 1); len(out) != 1 {
		t.Fatal(out)
	}
}

func TestPackPages(t *testing.T) {
	ctx := &plugin.CommandContext{}
	var rs []result
	for i := 0; i < 15; i++ {
		rs = append(rs, result{Title: strings.Repeat("Title_*[", 20), URL: "https://example.com/" + strings.Repeat("p", 50), Display: "example.com", Snippet: strings.Repeat("snippet text ", 40), Source: "ddg-html"})
	}
	pages := packPages(ctx, "q", bundle{results: rs, sources: []string{"DuckDuckGo"}}, time.Second)
	if len(pages) < 2 {
		t.Fatalf("expected multiple pages, got %d", len(pages))
	}
	count := 0
	for _, p := range pages {
		if l := plainLen(p); l > pageHardLimit {
			t.Fatalf("page too long: %d", l)
		}
		plain, ents := plugin.ParseMarkdown(p, nil)
		if len(ents) == 0 {
			t.Fatal("no entities: markdown parse failed?\n" + p)
		}
		count += strings.Count(plain, "\n› ")
	}
	if count != 15 {
		t.Fatalf("results across pages = %d", count)
	}
	empty := packPages(ctx, "q", bundle{notes: []string{"html: blocked"}}, time.Second)
	if len(empty) != 1 || !strings.Contains(empty[0], "blocked") {
		t.Fatal(empty)
	}
}
