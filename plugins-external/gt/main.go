package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	maxInputChars = 5000 // reference limit
	previewChars  = 50   // reference shows a 50-char preview of the source
	chunkRunes    = 3500 // Telegram caps messages at 4096 UTF-16 units
)

var Metadata = &plugin.PluginMetadata{
	Name:        "gt",
	Description: "谷歌翻译",
	DescEN:      "Google Translate",
	Version:     "1.1.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type GtPlugin struct {
	http *http.Client
}

func New() *GtPlugin {
	return &GtPlugin{http: &http.Client{Timeout: 10 * time.Second}}
}

func (p *GtPlugin) Name() string        { return "gt" }
func (p *GtPlugin) Description() string { return Metadata.Description }
func (p *GtPlugin) DescEN() string      { return Metadata.DescEN }

func (p *GtPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "gt",
		Aliases:     []string{"translate", "翻译"},
		Description: "谷歌翻译（默认译为中文，可指定目标语言，支持回复消息翻译）",
		DescEN:      "Google Translate (Chinese by default, optional target language, works on replies)",
		Usage:       "gt [目标语言] <文本> | 回复消息 gt [目标语言] | gt help",
		UsageEN:     "gt [target] <text> | reply with gt [target] | gt help",
		Plugin:      p.Name(),
		Category:    "tools",
		Handler:     p.handleTranslate,
	})
}

func (p *GtPlugin) Start(ctx context.Context) error { return nil }
func (p *GtPlugin) Stop(ctx context.Context) error  { return nil }

// ---------------------------------------------------------------- languages

// langNames lists Google Translate codes accepted as the target argument.
var langNames = map[string][2]string{
	"zh-CN": {"中文（简体）", "Chinese (Simplified)"}, "zh-TW": {"中文（繁体）", "Chinese (Traditional)"},
	"en": {"英文", "English"}, "ja": {"日文", "Japanese"}, "ko": {"韩文", "Korean"},
	"fr": {"法文", "French"}, "de": {"德文", "German"}, "es": {"西班牙文", "Spanish"},
	"ru": {"俄文", "Russian"}, "pt": {"葡萄牙文", "Portuguese"}, "it": {"意大利文", "Italian"},
	"ar": {"阿拉伯文", "Arabic"}, "th": {"泰文", "Thai"}, "vi": {"越南文", "Vietnamese"},
	"id": {"印尼文", "Indonesian"}, "ms": {"马来文", "Malay"}, "tr": {"土耳其文", "Turkish"},
	"nl": {"荷兰文", "Dutch"}, "pl": {"波兰文", "Polish"}, "uk": {"乌克兰文", "Ukrainian"},
	"hi": {"印地文", "Hindi"}, "fa": {"波斯文", "Persian"}, "he": {"希伯来文", "Hebrew"},
	"iw": {"希伯来文", "Hebrew"}, "sv": {"瑞典文", "Swedish"}, "fi": {"芬兰文", "Finnish"},
	"da": {"丹麦文", "Danish"}, "no": {"挪威文", "Norwegian"}, "cs": {"捷克文", "Czech"},
	"el": {"希腊文", "Greek"}, "hu": {"匈牙利文", "Hungarian"}, "ro": {"罗马尼亚文", "Romanian"},
	"bg": {"保加利亚文", "Bulgarian"}, "sr": {"塞尔维亚文", "Serbian"}, "hr": {"克罗地亚文", "Croatian"},
	"sk": {"斯洛伐克文", "Slovak"}, "sl": {"斯洛文尼亚文", "Slovenian"}, "lt": {"立陶宛文", "Lithuanian"},
	"lv": {"拉脱维亚文", "Latvian"}, "et": {"爱沙尼亚文", "Estonian"}, "bn": {"孟加拉文", "Bengali"},
	"ta": {"泰米尔文", "Tamil"}, "ur": {"乌尔都文", "Urdu"}, "fil": {"菲律宾文", "Filipino"},
	"tl": {"菲律宾文", "Filipino"}, "km": {"高棉文", "Khmer"}, "lo": {"老挝文", "Lao"},
	"my": {"缅甸文", "Burmese"}, "mn": {"蒙古文", "Mongolian"}, "kk": {"哈萨克文", "Kazakh"},
	"uz": {"乌兹别克文", "Uzbek"}, "ka": {"格鲁吉亚文", "Georgian"}, "hy": {"亚美尼亚文", "Armenian"},
	"az": {"阿塞拜疆文", "Azerbaijani"}, "be": {"白俄罗斯文", "Belarusian"}, "ca": {"加泰罗尼亚文", "Catalan"},
	"eo": {"世界语", "Esperanto"}, "la": {"拉丁文", "Latin"}, "sw": {"斯瓦希里文", "Swahili"},
	"af": {"南非荷兰文", "Afrikaans"}, "ga": {"爱尔兰文", "Irish"}, "is": {"冰岛文", "Icelandic"},
	"yue": {"粤语", "Cantonese"}, "lzh": {"文言文", "Classical Chinese"},
}

// aliases maps friendly spellings to Google codes.
var aliases = map[string]string{
	"zh": "zh-CN", "cn": "zh-CN", "zh-cn": "zh-CN", "zh-hans": "zh-CN", "chs": "zh-CN", "中文": "zh-CN", "简体": "zh-CN",
	"tw": "zh-TW", "zh-tw": "zh-TW", "zh-hant": "zh-TW", "cht": "zh-TW", "繁体": "zh-TW",
	"jp": "ja", "kr": "ko", "英文": "en", "英语": "en", "日文": "ja", "日语": "ja", "韩文": "ko", "韩语": "ko",
	"俄语": "ru", "法语": "fr", "德语": "de", "西班牙语": "es",
}

// ambiguous codes are also everyday English words; as a bare first word they
// are only treated as a target when nothing else follows (reply mode).
// Use "-t it" / "to:it" to force them.
var ambiguous = map[string]bool{
	"is": true, "it": true, "no": true, "my": true, "be": true, "hi": true,
	"id": true, "la": true, "ca": true, "tl": true, "ga": true, "af": true,
}

// normalizeLang returns the Google code for s, or "" when unknown.
func normalizeLang(s string) string {
	l := strings.TrimSpace(s)
	if v, ok := aliases[strings.ToLower(l)]; ok {
		return v
	}
	for code := range langNames {
		if strings.EqualFold(code, l) {
			return code
		}
	}
	return ""
}

func langLabel(ctx *plugin.CommandContext, code string) string {
	if code == "" {
		return "?"
	}
	if n, ok := langNames[code]; ok {
		return ctx.Tlocal(n[0], n[1])
	}
	if c := normalizeLang(code); c != "" {
		n := langNames[c]
		return ctx.Tlocal(n[0], n[1])
	}
	return code
}

// parseArgs splits args into an explicit target (may be "") and text.
func parseArgs(args []string) (target, text string, explicit bool) {
	rest := args
	if len(rest) >= 2 && (rest[0] == "-t" || rest[0] == "--to" || rest[0] == "-l") {
		if c := normalizeLang(rest[1]); c != "" {
			target, rest, explicit = c, rest[2:], true
		}
	} else if len(rest) >= 1 {
		first := rest[0]
		forced := false
		for _, pfx := range []string{"to:", ">", ":"} {
			if strings.HasPrefix(strings.ToLower(first), pfx) && len(first) > len(pfx) {
				first, forced = first[len(pfx):], true
				break
			}
		}
		if c := normalizeLang(first); c != "" && (forced || len(rest) == 1 || !ambiguous[strings.ToLower(first)]) {
			target, rest, explicit = c, rest[1:], true
		}
	}
	return target, strings.TrimSpace(strings.Join(rest, " ")), explicit
}

// ---------------------------------------------------------------- handler

func (p *GtPlugin) help(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"🌐 **谷歌翻译**\n\n"+
			"**基本用法**\n"+
			plugin.Code("gt <文本>")+" 翻译为中文（默认；原文是中文时译为英文）\n"+
			plugin.Code("gt en <文本>")+" 翻译为英文\n"+
			plugin.Code("gt ja <文本>")+" 翻译为指定语言\n\n"+
			"**回复消息翻译**\n"+
			plugin.Code("gt")+" 或 "+plugin.Code("gt en")+"（回复一条消息）\n\n"+
			"**目标语言**\n"+
			"zh / tw / en / ja / ko / fr / de / es / ru …\n"+
			"与英文单词冲突的代码（如 it、no）请用 "+plugin.Code("gt -t it <文本>")+"\n\n"+
			"💡 文本最长 5000 字符",
		"🌐 **Google Translate**\n\n"+
			"**Basic**\n"+
			plugin.Code("gt <text>")+" translate to Chinese (default; Chinese input goes to English)\n"+
			plugin.Code("gt en <text>")+" translate to English\n"+
			plugin.Code("gt ja <text>")+" translate to any language\n\n"+
			"**Reply mode**\n"+
			plugin.Code("gt")+" or "+plugin.Code("gt en")+" while replying to a message\n\n"+
			"**Targets**\n"+
			"zh / tw / en / ja / ko / fr / de / es / ru …\n"+
			"Codes that are English words (it, no …) need "+plugin.Code("gt -t it <text>")+"\n\n"+
			"💡 Text is limited to 5000 characters")
}

func (p *GtPlugin) handleTranslate(ctx *plugin.CommandContext) error {
	isReply := ctx.Message != nil && ctx.Message.IsReply && ctx.Message.ReplyToID > 0
	if len(ctx.Args) == 1 {
		if a := strings.ToLower(ctx.Args[0]); a == "help" || a == "h" {
			return ctx.Edit(p.help(ctx))
		}
	}
	if len(ctx.Args) == 0 && !isReply {
		return ctx.Edit(p.help(ctx))
	}

	target, text, explicit := parseArgs(ctx.Args)
	if text == "" && !isReply && len(ctx.Args) == 1 && ambiguous[strings.ToLower(ctx.Args[0])] {
		// "gt no" outside reply mode: the lone word is the text, not a target.
		target, text, explicit = "", ctx.Args[0], false
	}
	if text == "" {
		if !isReply {
			return ctx.Edit("❌ " + ctx.Tlocal("请提供要翻译的文本或回复一条消息", "Provide text or reply to a message"))
		}
		replyText, err := p.replyText(ctx)
		if err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("请提供要翻译的文本（无法获取回复消息）", "Provide text (could not load the replied message)"))
		}
		if strings.TrimSpace(replyText) == "" {
			return ctx.Edit("❌ " + ctx.Tlocal("回复的消息没有可翻译的文本", "The replied message has no text"))
		}
		text = strings.TrimSpace(replyText)
	}
	if n := utf8.RuneCountInString(text); n > maxInputChars {
		return ctx.Edit("❌ " + ctx.Tlocal(
			fmt.Sprintf("文本过长（%d 字符），请保持在 %d 字符以内", n, maxInputChars),
			fmt.Sprintf("Text too long (%d chars), keep it under %d", n, maxInputChars)))
	}
	if target == "" {
		target = "zh-CN"
		if mostlyChinese(text) {
			target = "en"
		}
	}

	_ = ctx.Edit("🔄 " + ctx.Tlocal("翻译中…", "Translating…"))

	res, err := p.translate(ctx.Context(), text, target)
	// Default target was Chinese but the source turned out Chinese already:
	// flip to English so the user gets something useful.
	if err == nil && !explicit && target == "zh-CN" && strings.HasPrefix(strings.ToLower(res.detected), "zh") {
		target = "en"
		res, err = p.translate(ctx.Context(), text, target)
	}
	if err != nil {
		if ctx.Logger != nil {
			ctx.Logger.Warn("gt: translate failed", "err", err)
		}
		return ctx.Edit("❌ " + ctx.Tlocal("翻译失败：", "Translation failed: ") + plugin.Escape(classify(ctx, err)))
	}
	if strings.TrimSpace(res.text) == "" {
		return ctx.Edit("❌ " + ctx.Tlocal("翻译结果为空或格式错误", "Empty translation result"))
	}

	preview := text
	if r := []rune(preview); len(r) > previewChars {
		preview = string(r[:previewChars]) + "..."
	}
	header := "🌐 **" + ctx.Tlocal("翻译结果", "Translation") + "**\n\n" +
		ctx.Tlocal("语言", "Languages") + "  " + plugin.Code(langLabel(ctx, res.detected)+" → "+langLabel(ctx, target)) + "\n\n" +
		"**" + ctx.Tlocal("原文", "Original") + "**\n" + plugin.Code(preview) + "\n\n" +
		"**" + ctx.Tlocal("译文", "Translation") + "**\n"

	chunks := splitRunes(res.text, chunkRunes)
	if err := ctx.Edit(header + plugin.Escape(chunks[0])); err != nil {
		return err
	}
	for _, c := range chunks[1:] {
		if err := ctx.Reply(plugin.Escape(c)); err != nil {
			return err
		}
	}
	return nil
}

// replyText loads the replied-to message and returns its text or caption.
func (p *GtPlugin) replyText(ctx *plugin.CommandContext) (string, error) {
	if ctx.API == nil {
		return "", errors.New("no api")
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return "", err
	}
	id := ctx.Message.ReplyToID
	ref := []tg.InputMessageClass{&tg.InputMessageID{ID: id}}
	var res tg.MessagesMessagesClass
	if ch, ok := peer.(*tg.InputPeerChannel); ok {
		res, err = ctx.API.ChannelsGetMessages(ctx.Context(), &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
			ID:      ref,
		})
	} else {
		res, err = ctx.API.MessagesGetMessages(ctx.Context(), ref)
	}
	if err != nil {
		return "", err
	}
	var msgs []tg.MessageClass
	switch m := res.(type) {
	case *tg.MessagesMessages:
		msgs = m.Messages
	case *tg.MessagesMessagesSlice:
		msgs = m.Messages
	case *tg.MessagesChannelMessages:
		msgs = m.Messages
	}
	for _, m := range msgs {
		if msg, ok := m.(*tg.Message); ok && msg.ID == id {
			return msg.Message, nil
		}
	}
	return "", fmt.Errorf("message %d not found", id)
}

// ---------------------------------------------------------------- backends

type result struct {
	text     string
	detected string
}

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// translate tries several keyless Google endpoints in turn. The classic
// translate_a/single (gtx) endpoint is often 429-limited from server IPs, so
// it is tried last.
func (p *GtPlugin) translate(ctx context.Context, text, target string) (*result, error) {
	backends := []func(context.Context, string, string) (*result, error){
		p.viaDictChrome, p.viaBatchExecute, p.viaGtx,
	}
	var errs []string
	for _, b := range backends {
		r, err := b(ctx, text, target)
		if err == nil && strings.TrimSpace(r.text) != "" {
			return r, nil
		}
		if err == nil {
			err = errors.New("empty result")
		}
		errs = append(errs, err.Error())
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

func (p *GtPlugin) post(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// viaGtx: translate.googleapis.com/translate_a/single (client=gtx).
// Response: [[["translated","source",...],...], null, "detected", ...]
func (p *GtPlugin) viaGtx(ctx context.Context, text, target string) (*result, error) {
	q := url.Values{"client": {"gtx"}, "sl": {"auto"}, "tl": {target}, "dt": {"t"}}
	body, err := p.post(ctx, "https://translate.googleapis.com/translate_a/single?"+q.Encode(), url.Values{"q": {text}})
	if err != nil {
		return nil, fmt.Errorf("gtx: %w", err)
	}
	return parseGtx(body)
}

func parseGtx(body []byte) (*result, error) {
	var raw []any
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) == 0 {
		return nil, errors.New("gtx: bad response")
	}
	var sb strings.Builder
	if segs, ok := raw[0].([]any); ok {
		for _, seg := range segs {
			if parts, ok := seg.([]any); ok && len(parts) > 0 {
				if s, ok := parts[0].(string); ok {
					sb.WriteString(s)
				}
			}
		}
	}
	r := &result{text: sb.String()}
	if len(raw) > 2 {
		r.detected, _ = raw[2].(string)
	}
	return r, nil
}

// viaDictChrome: clients5.google.com/translate_a/t (client=dict-chrome-ex).
// Response: [["translated","detected"]] (or ["translated"] for fixed sl).
func (p *GtPlugin) viaDictChrome(ctx context.Context, text, target string) (*result, error) {
	q := url.Values{"client": {"dict-chrome-ex"}, "sl": {"auto"}, "tl": {target}}
	body, err := p.post(ctx, "https://clients5.google.com/translate_a/t?"+q.Encode(), url.Values{"q": {text}})
	if err != nil {
		return nil, fmt.Errorf("dict: %w", err)
	}
	return parseDict(body)
}

func parseDict(body []byte) (*result, error) {
	var raw []any
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) == 0 {
		return nil, errors.New("dict: bad response")
	}
	r := &result{}
	var sb strings.Builder
	for _, item := range raw {
		switch v := item.(type) {
		case string:
			sb.WriteString(v)
		case []any:
			if len(v) > 0 {
				if s, ok := v[0].(string); ok {
					sb.WriteString(s)
				}
			}
			if len(v) > 1 && r.detected == "" {
				r.detected, _ = v[1].(string)
			}
		}
	}
	r.text = sb.String()
	return r, nil
}

// viaBatchExecute: the translate.google.com web UI RPC (MkEWBc).
func (p *GtPlugin) viaBatchExecute(ctx context.Context, text, target string) (*result, error) {
	inner, _ := json.Marshal([]any{[]any{text, "auto", target, true}, []any{nil}})
	freq, _ := json.Marshal([]any{[]any{[]any{"MkEWBc", string(inner), nil, "generic"}}})
	body, err := p.post(ctx, "https://translate.google.com/_/TranslateWebserverUi/data/batchexecute?rpcids=MkEWBc&rt=c",
		url.Values{"f.req": {string(freq)}})
	if err != nil {
		return nil, fmt.Errorf("batchexecute: %w", err)
	}
	return parseBatch(body)
}

func parseBatch(body []byte) (*result, error) {
	bad := errors.New("batchexecute: bad response")
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `[["wrb.fr"`) {
			continue
		}
		var outer [][]any
		if err := json.Unmarshal([]byte(line), &outer); err != nil || len(outer) == 0 || len(outer[0]) < 3 {
			return nil, bad
		}
		payload, ok := outer[0][2].(string)
		if !ok {
			return nil, bad
		}
		var data []any
		if err := json.Unmarshal([]byte(payload), &data); err != nil || len(data) < 2 {
			return nil, bad
		}
		r := &result{}
		if len(data) > 2 {
			r.detected, _ = data[2].(string)
		}
		if r.detected == "" {
			if d0, ok := data[0].([]any); ok && len(d0) > 2 {
				r.detected, _ = d0[2].(string)
			}
		}
		// data[1][0][0][5] = [[segment, ..., ..., ..., ..., ..., source, ...], ...]
		segs := dig(data, 1, 0, 0, 5)
		list, ok := segs.([]any)
		if !ok {
			return nil, bad
		}
		var sb strings.Builder
		for i, s := range list {
			arr, ok := s.([]any)
			if !ok || len(arr) == 0 {
				continue
			}
			t, _ := arr[0].(string)
			// Segments are sentence-split; restore the separating space
			// Google drops between Latin sentences.
			if i > 0 && sb.Len() > 0 && t != "" && !strings.HasPrefix(t, "\n") {
				last, _ := utf8.DecodeLastRuneInString(sb.String())
				first, _ := utf8.DecodeRuneInString(t)
				if !unicode.IsSpace(last) && !isCJK(last) && !isCJK(first) {
					sb.WriteByte(' ')
				}
			}
			sb.WriteString(t)
		}
		r.text = sb.String()
		return r, nil
	}
	return nil, bad
}

func dig(v any, path ...int) any {
	for _, i := range path {
		arr, ok := v.([]any)
		if !ok || i >= len(arr) {
			return nil
		}
		v = arr[i]
	}
	return v
}

// ---------------------------------------------------------------- helpers

func classify(ctx *plugin.CommandContext, err error) string {
	s := err.Error()
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout(), strings.Contains(strings.ToLower(s), "timeout"), errors.Is(err, context.DeadlineExceeded):
		return ctx.Tlocal("翻译请求超时，请稍后重试", "Request timed out, retry later")
	case strings.Contains(s, "429") || strings.Contains(strings.ToLower(s), "rate limit"):
		return ctx.Tlocal("请求过于频繁，请稍后重试", "Rate limited, retry later")
	case strings.Contains(strings.ToLower(s), "dial") || strings.Contains(strings.ToLower(s), "no such host") || strings.Contains(strings.ToLower(s), "connection"):
		return ctx.Tlocal("网络连接失败，请检查网络连接", "Network error, check connectivity")
	}
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100]) + "..."
	}
	return ctx.Tlocal("翻译服务暂时不可用（", "Translation service unavailable (") + s + ctx.Tlocal("）", ")")
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

// mostlyChinese reports whether Han characters dominate the letters in s
// (kana/hangul present → not Chinese).
func mostlyChinese(s string) bool {
	han, letters := 0, 0
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r), unicode.Is(unicode.Hangul, r):
			return false
		case unicode.Is(unicode.Han, r):
			han++
			letters++
		case unicode.IsLetter(r):
			letters++
		}
	}
	return han > 0 && han*2 >= letters
}

// splitRunes cuts s into pieces of at most n runes, preferring newlines.
func splitRunes(s string, n int) []string {
	r := []rune(s)
	if len(r) <= n {
		return []string{s}
	}
	var out []string
	for len(r) > n {
		cut := n
		for i := n; i > n/2; i-- {
			if r[i-1] == '\n' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		r = r[cut:]
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}
