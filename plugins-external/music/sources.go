package main

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// source is one music bot the plugin can drive.
type source struct {
	Key    string // used as @key in commands and in settings
	Bot    string // username without @
	Name   string
	NameEN string
	// btn is the callback-data prefix of the "pick track n" buttons in the
	// bot's result list.
	btn string
	// platforms maps our platform names to the bot's own token; nil means
	// the bot searches one fixed catalogue.
	platforms map[string]string
	// get builds the text that should end with an audio message; search
	// builds the text that should end with a result list.
	get    func(kw, plat, quality string) string
	search func(kw, plat string) string
}

var neteaseID = regexp.MustCompile(`music\.163\.com/.*?(?:song\?id=|song/)(\d+)`)

var sources = []*source{
	{
		Key: "down", Bot: "Music163DownBot", Name: "MusicBot-Go", NameEN: "MusicBot-Go", btn: "music ",
		platforms: map[string]string{
			"qq": "qq", "kugou": "kugou", "kuwo": "kuwo", "netease": "netease", "migu": "migu",
			"bili": "bilibili", "apple": "applemusic", "spotify": "spotify", "ytm": "youtubemusic", "soda": "soda",
		},
		get: func(kw, plat, q string) string {
			return strings.TrimSpace(strings.Join([]string{"/music", kw, plat, q}, " "))
		},
		search: func(kw, plat string) string { return strings.TrimSpace("/search " + kw + " " + plat) },
	},
	{
		Key: "vmomo", Bot: "VmomoVBot", Name: "MusicFinder", NameEN: "MusicFinder", btn: "play",
		get:    func(kw, _, _ string) string { return kw },
		search: func(kw, _ string) string { return kw },
	},
	{
		Key: "163", Bot: "Music163bot", Name: "网易云音乐", NameEN: "NetEase Cloud Music", btn: "music ",
		platforms: map[string]string{"netease": ""},
		get: func(kw, _, _ string) string {
			if m := neteaseID.FindStringSubmatch(kw); m != nil {
				return "/music " + m[1]
			}
			if isDigits(kw) {
				return "/music " + kw
			}
			return "/search " + kw
		},
		search: func(kw, _ string) string { return "/search " + kw },
	},
	{
		Key: "v1", Bot: "music_v1bot", Name: "Music v1", NameEN: "Music v1", btn: "music_",
		platforms: map[string]string{"qq": "qq", "kugou": "kugou", "kuwo": "kuwo", "netease": "netease"},
		get:       func(kw, plat, _ string) string { return v1Command(kw, plat) },
		search:    func(kw, plat string) string { return v1Command(kw, plat) },
	},
	{
		Key: "vk", Bot: "vkmusic_bot", Name: "VK Music", NameEN: "VK Music", btn: "a:",
		get:    func(kw, _, _ string) string { return kw },
		search: func(kw, _ string) string { return kw },
	},
}

func v1Command(kw, plat string) string {
	if plat == "" {
		return "/search " + kw
	}
	return "/" + plat + " " + kw
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func sourceByKey(k string) *source {
	k = strings.ToLower(strings.TrimPrefix(k, "@"))
	for _, s := range sources {
		if k == s.Key || k == strings.ToLower(s.Bot) {
			return s
		}
	}
	return nil
}

// platformAliases maps what users type to our platform names.
// Short ambiguous forms (am, sp, kg …) are left out on purpose: they
// would swallow the first word of song titles.
var platformAliases = map[string]string{
	"qq": "qq", "qqmusic": "qq", "qq音乐": "qq",
	"kugou": "kugou", "酷狗": "kugou",
	"kuwo": "kuwo", "酷我": "kuwo",
	"netease": "netease", "网易云": "netease",
	"migu": "migu", "咪咕": "migu",
	"bilibili": "bili", "bili": "bili", "b站": "bili",
	"applemusic": "apple", "apple": "apple",
	"spotify": "spotify",
	"ytm":     "ytm", "youtubemusic": "ytm",
	"soda": "soda", "汽水": "soda",
}

// platformNames lists platforms in help order.
var platformNames = []string{"qq", "kugou", "kuwo", "netease", "migu", "bili", "apple", "spotify", "ytm", "soda"}

var platformLabels = map[string][2]string{
	"qq": {"QQ 音乐", "QQ Music"}, "kugou": {"酷狗", "Kugou"}, "kuwo": {"酷我", "Kuwo"}, "netease": {"网易云", "NetEase"},
	"migu": {"咪咕", "Migu"}, "bili": {"B 站", "Bilibili"}, "apple": {"Apple Music", "Apple Music"},
	"spotify": {"Spotify", "Spotify"}, "ytm": {"YouTube Music", "YouTube Music"}, "soda": {"汽水音乐", "Soda Music"},
}

func platformChoices() []plugin.Choice {
	out := []plugin.Choice{{Value: "", Label: "机器人默认", LabelEN: "Bot default"}}
	for _, n := range platformNames {
		l := platformLabels[n]
		out = append(out, plugin.Choice{Value: n, Label: l[0], LabelEN: l[1]})
	}
	return out
}

// request is a parsed music command.
type request struct {
	Search bool
	Pick   int // 1-based pick from the last list, 0 when unused
	Source *source
	Plat   string // our platform name, "" for the bot default
	Query  string
}

// parseArgs reads: [search] [@source] [platform] <keywords>, or a lone number.
// The platform word only counts when keywords follow it.
func parseArgs(args []string) request {
	var r request
	if len(args) > 0 && strings.EqualFold(args[0], "search") {
		r.Search = true
		args = args[1:]
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "@") {
		if s := sourceByKey(args[0]); s != nil {
			r.Source = s
			args = args[1:]
		}
	}
	if len(args) > 1 {
		if p, ok := platformAliases[strings.ToLower(args[0])]; ok {
			r.Plat = p
			args = args[1:]
		}
	}
	r.Query = strings.TrimSpace(strings.Join(args, " "))
	if !r.Search && r.Source == nil && r.Plat == "" && isDigits(r.Query) && len(r.Query) <= 2 {
		n := 0
		for _, c := range r.Query {
			n = n*10 + int(c-'0')
		}
		r.Pick = n
	}
	return r
}

// route picks the bot and its platform token. An explicit source must
// support the platform; otherwise the default is used when it can, else
// the first source that can.
func route(r request, def *source) (*source, string, bool) {
	supports := func(s *source) (string, bool) {
		if r.Plat == "" {
			return "", true
		}
		tok, ok := s.platforms[r.Plat]
		return tok, ok
	}
	if r.Source != nil {
		tok, ok := supports(r.Source)
		return r.Source, tok, ok
	}
	if tok, ok := supports(def); ok {
		return def, tok, true
	}
	for _, s := range sources {
		if tok, ok := supports(s); ok {
			return s, tok, true
		}
	}
	return nil, "", false
}

// ---- reading bot replies ----

func audioDoc(m *tg.Message) (*tg.Document, *tg.DocumentAttributeAudio) {
	md, ok := m.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, nil
	}
	d, ok := md.Document.(*tg.Document)
	if !ok {
		return nil, nil
	}
	for _, a := range d.Attributes {
		if au, ok := a.(*tg.DocumentAttributeAudio); ok {
			if au.Voice {
				return nil, nil
			}
			return d, au
		}
	}
	if strings.HasPrefix(d.MimeType, "audio/") {
		return d, &tg.DocumentAttributeAudio{}
	}
	return nil, nil
}

// pickButtons returns the bot's "pick track" buttons in display order.
func pickButtons(m *tg.Message, prefix string) []*tg.KeyboardButtonCallback {
	rm, ok := m.ReplyMarkup.(*tg.ReplyInlineMarkup)
	if !ok {
		return nil
	}
	var out []*tg.KeyboardButtonCallback
	for _, row := range rm.Rows {
		for _, b := range row.Buttons {
			if cb, ok := b.(*tg.KeyboardButtonCallback); ok && strings.HasPrefix(string(cb.Data), prefix) {
				out = append(out, cb)
			}
		}
	}
	return out
}

var listLine = regexp.MustCompile(`^\s*(?:\d{1,2}[.、)]|[1-9]\x{FE0F}?\x{20E3}|🔟)\s*(.+?)\s*$`)

// listTitles extracts numbered result lines; ad lines are not numbered
// and are skipped.
func listTitles(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if m := listLine.FindStringSubmatch(l); m != nil {
			out = append(out, clip(m[1], 64))
		}
	}
	return out
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

var failWords = []string{"失败", "未找到", "没有找到", "错误", "unavailable", "not found", "error", "failed", "no results", "nothing found"}

// isFailure reports whether a plain bot text (no media, no pick buttons)
// is an error rather than a progress note.
func isFailure(text string) bool {
	t := strings.ToLower(text)
	for _, w := range failWords {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}
