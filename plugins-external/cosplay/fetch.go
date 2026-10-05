// Site scraping for the cosplay plugin: random photo sets from
// cosplaytele.com, mirroring the TeleBox reference behavior.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	baseURL = "https://cosplaytele.com/"
	// The site rejects non-browser User-Agents; the reference sends this one.
	siteUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"

	maxImages      = 10
	defaultCount   = 1
	siteMaxPages   = 455
	fetchAttempts  = 6 // sets with no/too few images are retried with a new set
	requestTimeout = 30 * time.Second
)

// photoSet is one gallery page on the site.
type photoSet struct {
	URL   string
	Title string
}

// supportedExt reports whether a URL path points at a raster image.
func supportedExt(u *url.URL) bool {
	switch strings.ToLower(path.Ext(u.Path)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return true
	}
	return false
}

// ---------------------------------------------------------------- HTML parsing

var (
	linkRe    = regexp.MustCompile(`(?i)<a\s[^>]*href\s*=\s*["']([^"']*)["']`)
	spaceRe   = regexp.MustCompile(`\s+`)
	galleryRe = regexp.MustCompile(`(?is)<figure[^>]*class=["'][^"']*gallery-item[^"']*["'][^>]*>.*?</figure>`)
	imgSrcRe  = regexp.MustCompile(`(?i)<img[^>]+?(?:data-src|src)\s*=\s*["']([^"']+)["']`)
	videoRe   = regexp.MustCompile(`(?i)<video[^>]*>|<iframe[^>]*>|<embed[^>]*>`)
)

// titleFromLink derives the display title from a photo set URL slug, like
// the reference: last path segment with dashes turned into spaces.
func titleFromLink(link string) string {
	parts := strings.FieldsFunc(link, func(r rune) bool { return r == '/' })
	if len(parts) == 0 {
		return "cosplay"
	}
	last := strings.TrimSuffix(parts[len(parts)-1], "tele.com")
	last = strings.TrimSuffix(last, "tele")
	return strings.TrimSpace(spaceRe.ReplaceAllString(strings.ReplaceAll(last, "-", " "), " "))
}

var junkPathRe = regexp.MustCompile(`/(page|category|24-hours|3-day|7-day|explore-categories|best-cosplayer|comments|feed|tag|author|wp-|wp-content|wp-includes)(/|$)`)

// isJunkLink filters navigational WordPress links, like the reference's
// regex over (page|category|24-hours|…|best-cosplayer).
func isJunkLink(u string) bool {
	if strings.HasSuffix(u, "/feed/") || strings.HasSuffix(u, ".jpg") ||
		strings.HasSuffix(u, ".png") || strings.HasSuffix(u, ".webp") {
		return true
	}
	return junkPathRe.MatchString(u)
}

// extractLinks returns candidate photo set URLs from a listing page.
func extractLinks(html string) []string {
	domain := domainOrHost(baseURL)
	seen := map[string]bool{}
	var out []string
	for _, m := range linkRe.FindAllStringSubmatch(html, -1) {
		href := strings.TrimSpace(m[1])
		if href == "" || strings.Contains(href, "#") ||
			strings.HasPrefix(strings.ToLower(href), "javascript:") {
			continue
		}
		if !strings.Contains(href, domain) {
			continue
		}
		var full string
		switch {
		case strings.HasPrefix(href, "http://"), strings.HasPrefix(href, "https://"):
			full = href
		case strings.HasPrefix(href, "/"):
			full = strings.TrimSuffix(baseURL, "/") + href
		default:
			continue
		}
		if full == baseURL || isJunkLink(full) || seen[full] {
			continue
		}
		seen[full] = true
		out = append(out, full)
	}
	return out
}

// extractGalleryImages pulls the gallery image URLs out of a photo set
// page: <figure class="gallery-item"><img src=…> like the reference.
func extractGalleryImages(html string) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range galleryRe.FindAllString(html, -1) {
		for _, s := range imgSrcRe.FindAllStringSubmatch(b, -1) {
			src := strings.TrimSpace(s[1])
			if src == "" || !strings.HasPrefix(src, "http") {
				continue
			}
			u, err := url.Parse(src)
			if err != nil || !supportedExt(u) {
				continue
			}
			if !seen[src] {
				seen[src] = true
				out = append(out, src)
			}
		}
	}
	// Some sets embed images outside gallery blocks: fall back to every
	// site upload image, but never when the set only contains videos.
	if len(out) == 0 && !videoRe.MatchString(html) {
		for _, m := range imgSrcRe.FindAllStringSubmatch(html, -1) {
			src := strings.TrimSpace(m[1])
			if src == "" || !strings.HasPrefix(src, "http") {
				continue
			}
			u, err := url.Parse(src)
			if err != nil || !supportedExt(u) {
				continue
			}
			if u.Hostname() != domainOrHost(baseURL) || !strings.Contains(u.Path, "/wp-content/uploads/") {
				continue
			}
			if !seen[src] {
				seen[src] = true
				out = append(out, src)
			}
		}
	}
	return out
}

// domainOrHost extracts the host part of an http(s) URL.
func domainOrHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// pickRandom selects k distinct random items from arr (the whole slice
// when k ≥ len), like the reference's pickRandom.
func pickRandom[T any](arr []T, k int) []T {
	if k >= len(arr) {
		return append([]T(nil), arr...)
	}
	idx := rand.Perm(len(arr))[:k]
	out := make([]T, 0, k)
	for _, i := range idx {
		out = append(out, arr[i])
	}
	return out
}

// ---------------------------------------------------------------- fetch

// errNoSet is returned when no suitable photo set could be found after
// several attempts.
var errNoSet = errors.New("no suitable photo set found")

// fromSite picks a random photo set and returns k image URLs from it,
// skipping sets without enough images (like the reference's fetchImageUrls).
func (p *CosplayPlugin) fromSite(ctx context.Context, k int) (*photoSet, []string, error) {
	var lastErr error
	for attempt := 0; attempt < fetchAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		ps, err := p.randomPhotoSet(ctx)
		if err != nil {
			lastErr = err
			p.sleep(ctx, time.Duration(attempt+1)*time.Second)
			continue
		}
		html, err := p.fetchHTML(ctx, ps.URL)
		if err != nil {
			lastErr = err
			p.sleep(ctx, time.Duration(attempt+1)*time.Second)
			continue
		}
		imgs := extractGalleryImages(html)
		if len(imgs) == 0 {
			lastErr = fmt.Errorf("%s: no images in set", ps.Title)
			continue
		}
		if len(imgs) < k {
			lastErr = fmt.Errorf("%s: only %d images", ps.Title, len(imgs))
			continue
		}
		return ps, pickRandom(imgs, k), nil
	}
	if lastErr == nil {
		lastErr = errNoSet
	}
	return nil, nil, fmt.Errorf("%w (%d attempts: %v)", errNoSet, fetchAttempts, lastErr)
}

// randomPhotoSet opens a random listing page and picks one of its photo
// set links at random.
func (p *CosplayPlugin) randomPhotoSet(ctx context.Context) (*photoSet, error) {
	page := rand.IntN(siteMaxPages) + 1
	pageURL := baseURL
	if page > 1 {
		pageURL = fmt.Sprintf("%spage/%d/", baseURL, page)
	}
	html, err := p.fetchHTML(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	links := extractLinks(html)
	if len(links) == 0 {
		return nil, fmt.Errorf("page %d: no photo set links", page)
	}
	link := links[rand.IntN(len(links))]
	return &photoSet{URL: link, Title: titleFromLink(link)}, nil
}

// fetchHTML downloads a page with the browser User-Agent (4 MB cap).
func (p *CosplayPlugin) fetchHTML(ctx context.Context, u string) (string, error) {
	hctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", siteUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := p.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d for %s", resp.StatusCode, u)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// sleep waits for d or until the context is cancelled.
func (p *CosplayPlugin) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ---------------------------------------------------------------- args

// parseCount converts a count argument. The bool is false when the text is
// not a positive number; values above the maximum are clamped.
func parseCount(s string) (int, bool) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil || n <= 0 {
		return 0, false
	}
	if n > maxImages {
		n = maxImages
	}
	return n, true
}

// clampCount applies the default and the upper bound.
func clampCount(n int) int {
	if n <= 0 {
		return defaultCount
	}
	if n > maxImages {
		return maxImages
	}
	return n
}
