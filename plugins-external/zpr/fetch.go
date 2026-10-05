// Lolicon API access, mirror (proxy) handling and image downloading for
// the zpr plugin, ported from the TeleBox zpr plugin.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	downloadTimeout = 30 * time.Second // per image, per mirror (like the source)
	// downloadAllTimeout bounds the whole batch: 10 images × 4 mirrors ×
	// 30s each would be minutes in the worst case.
	downloadAllTimeout = 3 * time.Minute
	dlConcurrency      = 3
)

// proxyHosts are the download mirrors, in panel order. i.pximg.net is
// Pixiv's own host and needs a Referer; the others are public mirrors.
var proxyHosts = []string{"i.pximg.net", "i.pixiv.cat", "i.pixiv.re", "i.pixiv.nl"}

// defaultProxy is i.pximg.net, the official image host.
const defaultProxy = "i.pximg.net"

// proxyChoices lists the mirrors for the settings panel.
func proxyChoices() []plugin.Choice {
	return []plugin.Choice{
		{Value: "i.pximg.net", Label: "官方 i.pximg.net", LabelEN: "Official i.pximg.net"},
		{Value: "i.pixiv.cat", Label: "反代 i.pixiv.cat", LabelEN: "Mirror i.pixiv.cat"},
		{Value: "i.pixiv.re", Label: "反代 i.pixiv.re", LabelEN: "Mirror i.pixiv.re"},
		{Value: "i.pixiv.nl", Label: "反代 i.pixiv.nl", LabelEN: "Mirror i.pixiv.nl"},
	}
}

// proxyOrder puts the configured mirror first, the others after it as
// fallbacks (like the source's [current, ...others] list).
func proxyOrder(preferred string) []string {
	out := make([]string, 0, len(proxyHosts))
	out = append(out, preferred)
	for _, h := range proxyHosts {
		if h != preferred {
			out = append(out, h)
		}
	}
	return out
}

// ---------------------------------------------------------------- API

// setuItem is one artwork from the Lolicon API v2 response.
type setuItem struct {
	PID    int      `json:"pid"`
	Title  string   `json:"title"`
	Author string   `json:"author"`
	Tags   []string `json:"tags"`
	Ext    string   `json:"ext"`
	URLs   struct {
		Original string `json:"original"`
		Regular  string `json:"regular"`
	} `json:"urls"`
}

// setuResponse is the Lolicon API envelope; success is HTTP 200 with an
// empty error field (the Next source checked a nonexistent code field).
type setuResponse struct {
	Error string     `json:"error"`
	Data  []setuItem `json:"data"`
}

// fetch queries the Lolicon API for req's count/r18/tag.
func (p *ZprPlugin) fetch(ctx context.Context, req setuRequest) ([]setuItem, error) {
	q := url.Values{}
	q.Set("num", strconv.Itoa(req.num))
	if req.r18 {
		q.Set("r18", "1")
	} else {
		q.Set("r18", "0")
	}
	if req.tag != "" {
		q.Set("tag", req.tag)
	}
	q.Add("size", "original")
	q.Add("size", "regular")

	hctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(hctx, http.MethodGet, apiURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("User-Agent", userAgent)
	hreq.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out setuResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("JSON: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("API: %s", out.Error)
	}
	if len(out.Data) == 0 {
		return nil, &localeError{zh: "未找到符合条件的图片", en: "No images matched"}
	}
	return out.Data, nil
}

// ---------------------------------------------------------------- host rewrite

// replaceHost swaps the host segment of rawURL for proxyHost. Only whole
// known pixiv hosts are rewritten: the Next source's substring replace
// turned i.pximg.net into i.i.pximg.net, so match from "://" to the first
// separator instead.
func replaceHost(rawURL, proxyHost string) string {
	schemeEnd := strings.Index(rawURL, "://")
	if schemeEnd < 0 {
		return rawURL
	}
	rest := rawURL[schemeEnd+3:]
	var hostSeg, tail string
	if sep := strings.IndexAny(rest, "/?#"); sep >= 0 {
		hostSeg, tail = rest[:sep], rest[sep:]
	} else {
		hostSeg = rest
	}
	if !knownPixivHost(hostSeg) {
		return rawURL
	}
	return rawURL[:schemeEnd+3] + proxyHost + tail
}

// knownPixivHost reports whether h (with optional port) is a host we rewrite.
func knownPixivHost(h string) bool {
	h = strings.ToLower(h)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	for _, k := range proxyHosts {
		if h == k {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- download

// imageFile is one downloaded artwork ready to send.
type imageFile struct {
	Path string
	Item setuItem
}

type downloadResult struct {
	path       string
	used       string
	prefFailed bool // the configured mirror failed for this image
	err        error
}

// downloadAll downloads every item (concurrently, three at a time) into
// data/zpr/tmp/, trying mirrors in order per image; the configured mirror
// first, the rest as fallbacks. Images whose every mirror failed are
// skipped; an error is returned only when nothing downloaded. When the
// configured mirror keeps failing while another works, it is switched
// persistently (Classic feature).
func (p *ZprPlugin) downloadAll(ctx context.Context, items []setuItem, preferred string) ([]imageFile, error) {
	if preferred == "" {
		preferred = defaultProxy
	}
	// Overall budget: with per-image mirror fallbacks the worst case is
	// minutes; cap the whole batch so the user is not stuck on progress
	// text forever.
	dctx, cancel := context.WithTimeout(ctx, downloadAllTimeout)
	defer cancel()
	ctx = dctx
	dir := p.dataDir()
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o755); err != nil {
		return nil, fmt.Errorf("tmp dir: %w", err)
	}
	order := proxyOrder(preferred)

	results := make([]downloadResult, len(items))
	sem := make(chan struct{}, dlConcurrency)
	var wg sync.WaitGroup
	for i := range items {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = p.downloadOne(ctx, items[i], i, dir, order)
		}(i)
	}
	wg.Wait()

	var files []imageFile
	var firstErr error
	prefFails, prefOK := 0, 0
	altUse := map[string]int{}
	for i, r := range results {
		if r.prefFailed {
			prefFails++
		}
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			p.logWarn("zpr: download failed", "pid", items[i].PID, "err", r.err)
			continue
		}
		files = append(files, imageFile{Path: r.path, Item: items[i]})
		if r.used == preferred {
			prefOK++
		} else {
			altUse[r.used]++
		}
	}
	if len(files) == 0 {
		if firstErr == nil {
			firstErr = errors.New("no images downloaded")
		}
		return nil, &localeError{
			zh: "所有图片下载失败（已尝试所有镜像）：" + firstErr.Error(),
			en: "All downloads failed (every mirror was tried): " + firstErr.Error(),
		}
	}
	p.maybeSwitchProxy(preferred, prefFails, prefOK, altUse)
	return files, nil
}

// downloadOne tries every mirror in order until one serves the image.
func (p *ZprPlugin) downloadOne(ctx context.Context, item setuItem, idx int, dir string, order []string) downloadResult {
	src := item.URLs.Original
	if src == "" {
		src = item.URLs.Regular
	}
	if src == "" {
		return downloadResult{err: fmt.Errorf("pid %d: no image url", item.PID)}
	}
	tmp := filepath.Join(dir, "tmp")
	var lastErr error
	for i, host := range order {
		if err := ctx.Err(); err != nil {
			return downloadResult{prefFailed: i > 0 || lastErr != nil, err: err}
		}
		path, err := p.downloadVia(ctx, replaceHost(src, host), host, item, idx, tmp)
		if err == nil {
			return downloadResult{path: path, used: host}
		}
		lastErr = fmt.Errorf("%s: %w", host, err)
	}
	return downloadResult{prefFailed: true, err: lastErr}
}

// downloadVia streams u (already rewritten to host) into a temp file.
func (p *ZprPlugin) downloadVia(ctx context.Context, u, host string, item setuItem, idx int, dir string) (string, error) {
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	if host == defaultProxy {
		// The official host rejects requests without the site Referer.
		req.Header.Set("Referer", "https://www.pixiv.net/")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" &&
		!strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "application/octet-stream") {
		return "", fmt.Errorf("unexpected content type %s", ct)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d_%d_%06d.%s", item.PID, idx, rand.IntN(1000000), urlExt(u, item.Ext)))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxImage+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		os.Remove(path)
		return "", err
	case n == 0:
		os.Remove(path)
		return "", errors.New("empty image")
	case n > maxImage:
		os.Remove(path)
		return "", fmt.Errorf("image over %dMB", maxImage>>20)
	}
	return path, nil
}

// urlExt picks the file extension: the API's ext field, else the URL path,
// else jpg (the source's default). Anything that could escape the temp dir
// (path separators, ..) is stripped: the value comes from a remote API.
func urlExt(u, fallback string) string {
	e := strings.TrimLeft(strings.ToLower(fallback), ".")
	e = pathExtSegment(e)
	if e != "" {
		return e
	}
	if sep := strings.IndexAny(u, "?#"); sep >= 0 {
		u = u[:sep]
	}
	if e := pathExtSegment(strings.TrimPrefix(strings.ToLower(filepath.Ext(u)), ".")); e != "" {
		return e
	}
	return "jpg"
}

// pathExtSegment reduces a candidate extension to plain characters, dropping
// anything containing separators or traversal dots entirely.
func pathExtSegment(e string) string {
	if e == "" || strings.ContainsAny(e, "/\\") || strings.Contains(e, "..") {
		return ""
	}
	if len(e) > 10 { // not a plausible extension; ignore the value
		return ""
	}
	return e
}

// maybeSwitchProxy persists the best-performing mirror when the configured
// one fails repeatedly (two or more images, or every image) while another
// mirror succeeds — the Classic plugin's smart proxy switch, logged only.
func (p *ZprPlugin) maybeSwitchProxy(preferred string, prefFails, prefOK int, altUse map[string]int) {
	if prefFails == 0 || len(altUse) == 0 {
		return
	}
	if prefFails < 2 && prefOK > 0 {
		return // a single hiccup is not a reason to switch
	}
	best, bestN := "", 0
	for _, h := range proxyHosts { // deterministic order on ties
		if n := altUse[h]; n > bestN {
			best, bestN = h, n
		}
	}
	if best == "" || best == preferred {
		return
	}
	if p.set != nil {
		if err := p.set.Set("proxy", best); err != nil {
			p.logWarn("zpr: switching default proxy failed", "to", best, "err", err)
			return
		}
	}
	if p.log != nil {
		p.log.Info("zpr: switched default proxy",
			"from", preferred, "to", best, "preferredFailures", prefFails, "successesViaNew", bestN)
	}
}

// cleanup removes the downloaded temp files, best effort.
func (p *ZprPlugin) cleanup(files []imageFile) {
	for _, f := range files {
		os.Remove(f.Path)
	}
}

// dataDir is the plugin data dir (set in Init) with a test fallback.
func (p *ZprPlugin) dataDir() string {
	if p.dir != "" {
		return p.dir
	}
	return "data/" + commandName
}

func (p *ZprPlugin) logWarn(msg string, kv ...any) {
	if p.log != nil {
		p.log.Warn(msg, kv...)
	}
}
