package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	wallhavenAPI = "https://wallhaven.cc/api/v1/search"
	fallbackAPI  = "https://api.btstu.cn/sjbz/api.php"
	userAgent    = "PaperValet-Bot/1.1"
	dataDir      = "data/bizhi"

	apiTimeout      = 60 * time.Second
	downloadTimeout = 120 * time.Second
	maxImageBytes   = 50 << 20 // reference maxContentLength
	minFileSize     = 3 << 20  // prefer wallpapers ≥ 3MB
	maxPhotoBytes   = 10 << 20 // Telegram photo upload limit
)

var Metadata = &plugin.PluginMetadata{
	Name:        "bizhi",
	Description: "随机高品质壁纸",
	DescEN:      "Random high-quality wallpaper",
	Version:     "1.1.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type BizhiPlugin struct {
	http *http.Client
}

func New() *BizhiPlugin {
	// Per-request deadlines come from contexts (API vs. large downloads).
	return &BizhiPlugin{http: &http.Client{Timeout: downloadTimeout}}
}

func (p *BizhiPlugin) Name() string        { return "bizhi" }
func (p *BizhiPlugin) Description() string { return Metadata.Description }
func (p *BizhiPlugin) DescEN() string      { return Metadata.DescEN }

func (p *BizhiPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	sweepStale()
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "bizhi",
		Aliases:     []string{"wallpaper", "壁纸"},
		Description: "随机获取一张高品质壁纸（wallhaven，btstu 备用）",
		DescEN:      "Random high-quality wallpaper (wallhaven, btstu fallback)",
		Usage:       "bizhi [meizi|dongman|fengjing|suiji] [-f] | bizhi help",
		UsageEN:     "bizhi [meizi|dongman|fengjing|suiji] [-f] | bizhi help",
		Plugin:      p.Name(),
		Category:    "fun",
		Handler:     p.handleBizhi,
	})
}

func (p *BizhiPlugin) Start(ctx context.Context) error { return nil }
func (p *BizhiPlugin) Stop(ctx context.Context) error  { return nil }

// ---------------------------------------------------------------- categories

type category struct {
	wallhaven string // wallhaven categories bitmask ("" = all)
	tags      []string
}

var categories = map[string]category{
	"meizi":    {"001", []string{"photography", "portrait", "aesthetic"}},
	"dongman":  {"010", []string{"anime", "illustration", "digital painting", "Studio Ghibli", "anime screenshot"}},
	"fengjing": {"100", []string{"nature", "Japan", "night", "architecture", "oil painting", "photography"}},
	"suiji":    {"", []string{"anime", "oil painting", "photography", "Japan", "night", "illustration"}},
}

var defaultCategory = category{"", []string{"anime", "oil painting", "photography", "Japan", "night"}}

var categoryAliases = map[string]string{
	"meinv": "meizi", "美女": "meizi", "people": "meizi",
	"anime": "dongman", "动漫": "dongman",
	"scenery": "fengjing", "landscape": "fengjing", "风景": "fengjing",
	"random": "suiji", "随机": "suiji",
}

func normalizeCategory(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", true
	}
	if a, ok := categoryAliases[s]; ok {
		s = a
	}
	_, ok := categories[s]
	return s, ok
}

// ---------------------------------------------------------------- handler

func (p *BizhiPlugin) help(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"🖼 **随机壁纸**\n\n"+
			"**用法**\n"+
			plugin.Code("bizhi")+" 随机类型\n"+
			plugin.Code("bizhi dongman")+" 指定分类\n"+
			plugin.Code("bizhi fengjing -f")+" 以原文件发送\n\n"+
			"**分类**\n"+
			plugin.Code("meizi")+" 美女  "+plugin.Code("dongman")+" 动漫  "+plugin.Code("fengjing")+" 风景  "+plugin.Code("suiji")+" 随机\n\n"+
			"**说明**\n"+
			"优先 wallhaven.cc 原图（≥1920×1080，16:9，优先 ≥3MB），失败回退 btstu.cn\n"+
			"超过 Telegram 图片限制时自动改为文件发送\n\n"+
			"💡 "+plugin.Code("-f")+" 发送源文件而非图片",
		"🖼 **Random wallpaper**\n\n"+
			"**Usage**\n"+
			plugin.Code("bizhi")+" random category\n"+
			plugin.Code("bizhi dongman")+" pick a category\n"+
			plugin.Code("bizhi fengjing -f")+" send as original file\n\n"+
			"**Categories**\n"+
			plugin.Code("meizi")+" people  "+plugin.Code("dongman")+" anime  "+plugin.Code("fengjing")+" scenery  "+plugin.Code("suiji")+" random\n\n"+
			"**Details**\n"+
			"Original images from wallhaven.cc (≥1920×1080, 16:9, prefers ≥3MB), btstu.cn as fallback\n"+
			"Images over Telegram's photo limit are sent as files automatically\n\n"+
			"💡 "+plugin.Code("-f")+" sends the source file instead of a photo")
}

func (p *BizhiPlugin) handleBizhi(ctx *plugin.CommandContext) error {
	var cats []string
	sendAsFile := false
	for _, a := range ctx.Args {
		switch strings.ToLower(a) {
		case "help", "h", "-h", "--help":
			return ctx.Edit(p.help(ctx))
		case "-f", "--file", "-file":
			sendAsFile = true
		default:
			if !strings.HasPrefix(a, "-") {
				cats = append(cats, a)
			}
		}
	}
	lx := ""
	if len(cats) > 0 {
		var ok bool
		if lx, ok = normalizeCategory(cats[0]); !ok {
			return ctx.Edit("❌ " + ctx.Tlocal("未知分类：", "Unknown category: ") + plugin.Code(cats[0]) +
				"\n\n💡 " + ctx.Tlocal("可选：", "Choose: ") + plugin.Code("meizi dongman fengjing suiji"))
		}
	}
	if ctx.API == nil || ctx.Message == nil || ctx.Message.Message == nil {
		return ctx.Edit("❌ " + ctx.Tlocal("客户端未就绪", "Client not ready"))
	}

	_ = ctx.Edit("🖼 " + ctx.Tlocal("正在获取高品质壁纸…", "Fetching a high-quality wallpaper…"))

	wp, err := p.getWallpaper(ctx.Context(), lx)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("获取壁纸失败：", "Failed to get wallpaper: ") + plugin.Escape(trim(err.Error(), 300)))
	}
	defer cleanup(wp.path)

	_ = ctx.Edit("📤 " + ctx.Tlocal("正在上传壁纸…", "Uploading wallpaper…"))
	caption := "📸 " + ctx.Tlocal("来源: ", "Source: ") + wp.source
	if err := p.send(ctx, wp, caption, sendAsFile); err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("发送壁纸失败：", "Failed to send wallpaper: ") + plugin.Escape(trim(err.Error(), 300)))
	}
	_ = ctx.Delete()
	return nil
}

// ---------------------------------------------------------------- sources

type wallpaper struct {
	path   string // local temp file
	source string // caption text (URL + info)
	size   int64
	w, h   int
}

type whWallpaper struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	FileSize int64  `json:"file_size"`
	FileType string `json:"file_type"`
	DimX     int    `json:"dimension_x"`
	DimY     int    `json:"dimension_y"`
}

func randomSeed() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}

// buildWallhavenQuery mirrors the reference's mixed strategy: 60% random,
// 25% favourites, 15% newest; 1-2 random priority tags; occasional page jump.
func buildWallhavenQuery(c category) url.Values {
	r := rand.Float64()
	sorting := "date_added"
	switch {
	case r < 0.6:
		sorting = "random"
	case r < 0.85:
		sorting = "favorites"
	}
	q := url.Values{
		"sorting":  {sorting},
		"purity":   {"100"}, // SFW only
		"per_page": {"24"},
		"atleast":  {"1920x1080"},
		"ratios":   {"16x9"},
	}
	if sorting == "random" {
		q.Set("seed", randomSeed())
	} else {
		q.Set("order", "desc")
	}
	if len(c.tags) > 0 {
		tags := append([]string(nil), c.tags...)
		rand.Shuffle(len(tags), func(i, j int) { tags[i], tags[j] = tags[j], tags[i] })
		n := 1
		if rand.Float64() >= 0.7 && len(tags) > 1 {
			n = 2
		}
		q.Set("q", strings.Join(tags[:n], "+"))
	}
	if rand.Float64() < 0.2 {
		q.Set("page", fmt.Sprint(rand.IntN(3)+1))
	}
	if c.wallhaven != "" {
		q.Set("categories", c.wallhaven)
	}
	return q
}

func qualified(list []whWallpaper, minW, minH int) []whWallpaper {
	var out []whWallpaper
	for _, w := range list {
		if w.FileSize >= minFileSize && w.DimX >= minW && w.DimY >= minH {
			out = append(out, w)
		}
	}
	return out
}

func (p *BizhiPlugin) searchWallhaven(ctx context.Context, q url.Values) ([]whWallpaper, error) {
	var data struct {
		Data []whWallpaper `json:"data"`
	}
	if err := p.getJSON(ctx, wallhavenAPI+"?"+q.Encode(), &data); err != nil {
		return nil, err
	}
	return data.Data, nil
}

func (p *BizhiPlugin) pickWallhaven(ctx context.Context, c category) (*whWallpaper, error) {
	q := buildWallhavenQuery(c)
	list, err := p.searchWallhaven(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		// Two-tag combos or a random page can come back empty: retry once
		// with a single tag on page 1 before giving up.
		q.Del("page")
		if tags := strings.Split(q.Get("q"), "+"); len(tags) > 0 && tags[0] != "" {
			q.Set("q", tags[0])
		}
		if q.Get("sorting") == "random" {
			q.Set("seed", randomSeed())
		}
		if list, err = p.searchWallhaven(ctx, q); err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, errors.New("no wallpapers found")
		}
	}
	pool := qualified(list, 1920, 1080)
	if len(pool) > 0 {
		w := pool[rand.IntN(len(pool))]
		return &w, nil
	}
	// Nothing ≥3MB: retry once with a higher resolution floor.
	q.Set("atleast", "2560x1440")
	if q.Get("sorting") == "random" {
		q.Set("seed", randomSeed())
	}
	if retry, err := p.searchWallhaven(ctx, q); err == nil && len(retry) > 0 {
		pool = qualified(retry, 2560, 1440)
		if len(pool) == 0 {
			pool = retry
		}
		w := pool[rand.IntN(len(pool))]
		return &w, nil
	}
	w := list[rand.IntN(len(list))]
	return &w, nil
}

func (p *BizhiPlugin) getWallpaper(ctx context.Context, lx string) (*wallpaper, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	c, ok := categories[lx]
	if !ok {
		c = defaultCategory
	}
	wp, whErr := p.fromWallhaven(ctx, c)
	if whErr == nil {
		return wp, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	wp, fbErr := p.fromFallback(ctx, lx)
	if fbErr == nil {
		return wp, nil
	}
	return nil, fmt.Errorf("all sources failed: wallhaven(%v), fallback(%v)", whErr, fbErr)
}

func (p *BizhiPlugin) fromWallhaven(ctx context.Context, c category) (*wallpaper, error) {
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	w, err := p.pickWallhaven(actx, c)
	cancel()
	if err != nil {
		return nil, err
	}
	ext := ".jpg"
	switch w.FileType {
	case "image/png":
		ext = ".png"
	case "image/webp":
		ext = ".webp"
	}
	name := fmt.Sprintf("wallhaven_%s_%dx%d%s", safeName(w.ID), w.DimX, w.DimY, ext)
	path, size, err := p.download(ctx, w.Path, name, "https://wallhaven.cc/")
	if err != nil {
		return nil, err
	}
	return &wallpaper{
		path:   path,
		source: fmt.Sprintf("%s\n📊 %d×%d, %.2fMB", w.Path, w.DimX, w.DimY, float64(size)/1024/1024),
		size:   size, w: w.DimX, h: w.DimY,
	}, nil
}

// fromFallback uses btstu.cn, which knows meizi/dongman/fengjing/suiji.
func (p *BizhiPlugin) fromFallback(ctx context.Context, lx string) (*wallpaper, error) {
	q := url.Values{"method": {"pc"}, "format": {"json"}}
	if lx != "" {
		q.Set("lx", lx)
	}
	var data struct {
		Code   string `json:"code"`
		ImgURL string `json:"imgurl"`
		Width  string `json:"width"`
		Height string `json:"height"`
	}
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	err := p.getJSON(actx, fallbackAPI+"?"+q.Encode(), &data)
	cancel()
	if err != nil {
		return nil, err
	}
	if data.Code != "200" || data.ImgURL == "" {
		return nil, errors.New("fallback API returned no image")
	}
	name := "bizhi_" + safeName(firstNonEmpty(lx, "suiji")) + ".jpg"
	path, size, err := p.download(ctx, data.ImgURL, name, "")
	if err != nil {
		return nil, err
	}
	src := data.ImgURL + "\n📊 "
	if data.Width != "" && data.Height != "" {
		src += data.Width + "×" + data.Height + ", "
	}
	src += fmt.Sprintf("%.2fMB · btstu.cn", float64(size)/1024/1024)
	return &wallpaper{path: path, source: src, size: size}, nil
}

func (p *BizhiPlugin) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// download streams rawURL into a unique temp file under data/bizhi/.
func (p *BizhiPlugin) download(ctx context.Context, rawURL, name, referer string) (string, int64, error) {
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "image/webp,image/apng,image/*,*/*;q=0.8")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("image HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "application/octet-stream") {
		return "", 0, fmt.Errorf("unexpected content type %s", ct)
	}
	if resp.ContentLength > maxImageBytes {
		return "", 0, fmt.Errorf("image too large (%d bytes)", resp.ContentLength)
	}

	// Unique sub-directory keeps the friendly file name (used as the
	// document name with -f) while allowing concurrent invocations.
	dir, err := os.MkdirTemp(dataDir, "dl-")
	if err != nil {
		return "", 0, err
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		os.RemoveAll(dir)
		return "", 0, err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxImageBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxImageBytes {
		err = fmt.Errorf("image larger than %dMB", maxImageBytes>>20)
	}
	if err == nil && n == 0 {
		err = errors.New("empty image")
	}
	if err != nil {
		os.RemoveAll(dir)
		return "", 0, err
	}
	return path, n, nil
}

// cleanup removes the temp file together with its private directory.
func cleanup(path string) {
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	if filepath.Base(filepath.Dir(dir)) == filepath.Base(dataDir) && strings.HasPrefix(filepath.Base(dir), "dl-") {
		os.RemoveAll(dir)
		return
	}
	os.Remove(path)
}

// sweepStale removes temp downloads left behind by a crash or restart.
func sweepStale() {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "dl-") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
			os.RemoveAll(filepath.Join(dataDir, e.Name()))
		}
	}
}

// ---------------------------------------------------------------- sending

// send uploads the wallpaper as a photo, or as a document when -f is given
// or the file exceeds Telegram's 10MB photo limit / extreme aspect ratio.
func (p *BizhiPlugin) send(ctx *plugin.CommandContext, wp *wallpaper, caption string, asFile bool) error {
	if !asFile && wp.size > maxPhotoBytes {
		asFile = true
	}
	if !asFile && ctx.Media != nil {
		err := ctx.ReplyMedia(wp.path, caption)
		if err == nil {
			return nil
		}
		// Telegram rejects some photos (PHOTO_INVALID_DIMENSIONS, too big…);
		// retry as a document.
		if ctx.Logger != nil {
			ctx.Logger.Warn("bizhi: send as photo failed, retrying as file", "err", err)
		}
	}
	return p.sendDocument(ctx, wp.path, caption)
}

func (p *BizhiPlugin) sendDocument(ctx *plugin.CommandContext, path, caption string) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	file, err := uploader.NewUploader(ctx.API).FromPath(ctx.Context(), path)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	mt := mime.TypeByExtension(filepath.Ext(path))
	if mt == "" {
		mt = "image/jpeg"
	}
	req := &tg.MessagesSendMediaRequest{
		Peer: peer,
		Media: &tg.InputMediaUploadedDocument{
			ForceFile:  true,
			File:       file,
			MimeType:   mt,
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: filepath.Base(path)}},
		},
		Message:  caption,
		RandomID: rand.Int64(),
	}
	req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: ctx.Message.Message.ID})
	_, err = ctx.API.MessagesSendMedia(ctx.Context(), req)
	return err
}

// ---------------------------------------------------------------- helpers

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
