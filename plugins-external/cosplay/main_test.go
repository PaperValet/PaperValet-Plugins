package main

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestParseCount(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want bool
	}{
		{"5", 5, true},
		{" 3 ", 3, true},
		{"1", 1, true},
		{"10", 10, true},
		{"99", maxImages, true}, // clamped like the reference
		{"0", 0, false},
		{"-2", 0, false},
		{"", 0, false},
		{"abc", 0, false},
		{"2x", 2, true}, // parseInt semantics: leading digits count
	}
	for _, c := range cases {
		n, ok := parseCount(c.in)
		if ok != c.want || (ok && n != c.n) {
			t.Errorf("parseCount(%q) = %d, %v; want %d, %v", c.in, n, ok, c.n, c.want)
		}
	}
}

func TestClampCount(t *testing.T) {
	for in, want := range map[int]int{0: defaultCount, -1: defaultCount, 1: 1, 5: 5, 11: maxImages, 100: maxImages} {
		if got := clampCount(in); got != want {
			t.Errorf("clampCount(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestTitleFromLink(t *testing.T) {
	cases := map[string]string{
		"https://cosplaytele.com/aemeath-2/":           "aemeath 2",
		"https://cosplaytele.com/cheshire-cheongsam-2": "cheshire cheongsam 2",
		"https://cosplaytele.com":                      "cosplay",
		"https://cosplaytele.com/":                     "cosplay",
	}
	for in, want := range cases {
		if got := titleFromLink(in); got != want {
			t.Errorf("titleFromLink(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsJunkLink(t *testing.T) {
	junk := []string{
		"https://cosplaytele.com/page/3/",
		"https://cosplaytele.com/category/cosplay/",
		"https://cosplaytele.com/24-hours/",
		"https://cosplaytele.com/7-day/",
		"https://cosplaytele.com/explore-categories/",
		"https://cosplaytele.com/best-cosplayer/",
		"https://cosplaytele.com/comments/feed/",
		"https://cosplaytele.com/wp-content/uploads/x.jpg",
	}
	for _, u := range junk {
		if !isJunkLink(u) {
			t.Errorf("isJunkLink(%q) = false, want true", u)
		}
	}
	sets := []string{
		"https://cosplaytele.com/aemeath-2/",
		"https://cosplaytele.com/cheshire-10/",
		"https://cosplaytele.com/castorice-4/",
	}
	for _, u := range sets {
		if isJunkLink(u) {
			t.Errorf("isJunkLink(%q) = true, want false", u)
		}
	}
}

func TestExtractLinks(t *testing.T) {
	html := `
		<a href="https://cosplaytele.com/">home</a>
		<a href="https://cosplaytele.com/aemeath-2/">Aemeath</a>
		<a href="https://cosplaytele.com/aemeath-2/">dup</a>
		<a href="https://cosplaytele.com/page/2/">page</a>
		<a href="https://cosplaytele.com/category/cosplay/">cat</a>
		<a href="https://cosplaytele.com/cheshire-10/">Cheshire</a>
		<a href="#top">anchor</a>
		<a href="javascript:void(0)">js</a>
		<a href="https://example.com/other/">ext</a>
		<a href="https://cosplaytele.com/castorice-4">castorice</a>
	`
	links := extractLinks(html)
	want := []string{
		"https://cosplaytele.com/aemeath-2/",
		"https://cosplaytele.com/cheshire-10/",
		"https://cosplaytele.com/castorice-4",
	}
	if len(links) != len(want) {
		t.Fatalf("extractLinks = %v, want %v", links, want)
	}
	got := map[string]bool{}
	for _, l := range links {
		got[l] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %s in %v", w, links)
		}
	}
}

const galleryHTML = `
<div id='gallery-1' class='gallery galleryid-511315'>
<figure class='gallery-item'><div class='gallery-icon portrait'>
<a href='https://cosplaytele.com/wp-content/uploads/2026/06/Aqua-1_result.webp'>
<img width="1067" height="1600" src="https://cosplaytele.com/wp-content/uploads/2026/06/Aqua-1_result.webp" class="attachment-full size-full" alt="" /></a>
</div></figure>
<figure class='gallery-item'><div class='gallery-icon portrait'>
<img src="https://cosplaytele.com/wp-content/uploads/2026/06/Aqua-2_result.webp" />
</div></figure>
<figure class='gallery-item'>
<img data-src="https://cosplaytele.com/wp-content/uploads/2026/06/Aqua-3_result.jpeg" src="data:image/gif;base64,xxx" />
</figure>
</div>
<img src="https://cosplaytele.com/wp-content/uploads/2024/01/avatar.png" />
<video src="https://cosplaytele.com/video.mp4"></video>
`

func TestExtractGalleryImages(t *testing.T) {
	imgs := extractGalleryImages(galleryHTML)
	if len(imgs) != 3 {
		t.Fatalf("got %d images: %v", len(imgs), imgs)
	}
	want := []string{
		"https://cosplaytele.com/wp-content/uploads/2026/06/Aqua-1_result.webp",
		"https://cosplaytele.com/wp-content/uploads/2026/06/Aqua-2_result.webp",
		"https://cosplaytele.com/wp-content/uploads/2026/06/Aqua-3_result.jpeg",
	}
	for i, w := range want {
		if imgs[i] != w {
			t.Errorf("imgs[%d] = %s, want %s", i, imgs[i], w)
		}
	}
}

func TestExtractGalleryImagesVideoOnly(t *testing.T) {
	html := `<video src="https://cosplaytele.com/v.mp4"></video>
	<img src="https://cosplaytele.com/wp-content/uploads/2026/06/thumb.webp" />`
	if imgs := extractGalleryImages(html); len(imgs) != 0 {
		t.Errorf("video-only set should give no images, got %v", imgs)
	}
}

func TestExtractGalleryImagesFallback(t *testing.T) {
	// No gallery-item markup, but site images: fallback applies.
	html := `<p>intro</p><img src="https://cosplaytele.com/wp-content/uploads/2026/06/only-one.webp" />`
	imgs := extractGalleryImages(html)
	if len(imgs) != 1 || imgs[0] != "https://cosplaytele.com/wp-content/uploads/2026/06/only-one.webp" {
		t.Fatalf("got %v", imgs)
	}
}

func TestPickRandom(t *testing.T) {
	arr := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	for k := 0; k <= len(arr); k++ {
		got := pickRandom(arr, k)
		if len(got) != k {
			t.Fatalf("k=%d: got %d items", k, len(got))
		}
		seen := map[int]bool{}
		for _, v := range got {
			if seen[v] {
				t.Fatalf("k=%d: duplicate %d", k, v)
			}
			seen[v] = true
			if v < 1 || v > 10 {
				t.Fatalf("k=%d: out of range %d", k, v)
			}
		}
	}
	full := pickRandom(arr, len(arr))
	if len(full) != len(arr) {
		t.Fatal("k ≥ len should copy everything")
	}
}

func TestSupportedExt(t *testing.T) {
	for _, ok := range []string{".jpg", ".JPG", ".jpeg", ".png", ".webp"} {
		u, _ := url.Parse("https://x.com/a" + ok)
		if !supportedExt(u) {
			t.Errorf("%s should be supported", ok)
		}
	}
	for _, no := range []string{".mp4", ".gif", "", ".html"} {
		u, _ := url.Parse("https://x.com/a" + no)
		if supportedExt(u) {
			t.Errorf("%s should not be supported", no)
		}
	}
}

func TestRetryableDownload(t *testing.T) {
	if !retryableDownload(httpErr{status: 502, url: "u"}) {
		t.Error("5xx should retry")
	}
	if !retryableDownload(httpErr{status: 429, url: "u"}) {
		t.Error("429 should retry")
	}
	if retryableDownload(httpErr{status: 404, url: "u"}) {
		t.Error("404 should not retry")
	}
	if !retryableDownload(errors.New("boom")) {
		t.Error("network error should retry (like the reference transient class)")
	}
	if retryableDownload(context.Canceled) {
		t.Error("canceled should not retry")
	}
}

func TestDescribeErr(t *testing.T) {
	if got := describeErr(errors.New("plain")); got != "plain" {
		t.Errorf("got %q", got)
	}
	if got := describeErr(httpErr{status: 500, url: "u"}); !strings.Contains(got, "500") {
		t.Errorf("got %q", got)
	}
}

func TestTrimErr(t *testing.T) {
	if got := trimErr(strings.Repeat("x", 400)); len([]rune(got)) != 301 {
		t.Errorf("trimErr length = %d", len([]rune(got)))
	}
	if got := trimErr("short"); got != "short" {
		t.Errorf("got %q", got)
	}
}

func TestMaxImagesBound(t *testing.T) {
	if maxImages != 10 {
		t.Fatalf("maxImages = %d, reference cap is 10", maxImages)
	}
}
