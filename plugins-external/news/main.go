package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	apiURL = "https://news.topurl.cn/api"
	maxLen = 3800
)

var Metadata = &plugin.PluginMetadata{
	Name:        "news",
	Description: "每日新闻和历史上的今天",
	DescEN:      "Daily Chinese news digest",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// categories accepted by the API, with English names for input and help.
var categories = [][2]string{{"时事", "headlines"}, {"国内", "china"}, {"国际", "world"}, {"商业", "business"}}

type NewsPlugin struct {
	http *http.Client
	set  plugin.Settings

	cacheMu   sync.Mutex
	cacheKey  string
	cacheAt   time.Time
	cacheData *newsData
}

// newsCacheTTL: the digest is stable for the whole day; a few minutes keep
// repeated commands instant while still following panel changes.
const newsCacheTTL = 5 * time.Minute

// cached fetch: same count+category inside the TTL returns the earlier
// result without hitting the API again.
func (p *NewsPlugin) fetchCached(ctx context.Context, count int, cat string) (*newsData, error) {
	key := fmt.Sprintf("%d|%s", count, cat)
	p.cacheMu.Lock()
	if p.cacheData != nil && p.cacheKey == key && time.Since(p.cacheAt) < newsCacheTTL {
		d := p.cacheData
		p.cacheMu.Unlock()
		return d, nil
	}
	p.cacheMu.Unlock()

	d, err := p.fetch(ctx, count, cat)
	if err != nil {
		return nil, err
	}
	p.cacheMu.Lock()
	p.cacheKey, p.cacheAt, p.cacheData = key, time.Now(), d
	p.cacheMu.Unlock()
	return d, nil
}

func New() *NewsPlugin { return &NewsPlugin{http: &http.Client{Timeout: 15 * time.Second}} }

func (p *NewsPlugin) Name() string        { return "news" }
func (p *NewsPlugin) Description() string { return Metadata.Description }
func (p *NewsPlugin) DescEN() string      { return Metadata.DescEN }

func (p *NewsPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	set, err := mgr.Host().Settings(&plugin.SettingsSpec{
		Plugin: p.Name(), Title: "🗞️ 每日新闻", TitleEN: "🗞️ Daily news",
		Settings: []plugin.Setting{
			{Key: "count", Label: "新闻条数", LabelEN: "News items", Kind: plugin.SettingNumber, Default: 12, Min: 8, Max: 20,
				Hint: "接口只支持 8 到 20 条", HintEN: "The API allows 8 to 20 items"},
			{Key: "history", Label: "历史上的今天", LabelEN: "On this day", Kind: plugin.SettingToggle, Default: true},
			{Key: "phrase", Label: "天天成语", LabelEN: "Idiom of the day", Kind: plugin.SettingToggle, Default: true},
			{Key: "sentence", Label: "慧语香风", LabelEN: "Quote of the day", Kind: plugin.SettingToggle, Default: true},
			{Key: "poem", Label: "诗歌天地", LabelEN: "Poem of the day", Kind: plugin.SettingToggle, Default: true},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "news",
		Description: "每日新闻、历史上的今天、成语、名言和诗词，可按类目筛选",
		DescEN:      "Daily news, on-this-day, idiom, quote and poem (Chinese), optionally by category",
		Usage:       "news [时事|国内|国际|商业]",
		UsageEN:     "news [headlines|china|world|business]",
		Plugin:      p.Name(),
		Category:    "info",
		Handler:     p.handle,
	})
}

func (p *NewsPlugin) Start(context.Context) error { return nil }
func (p *NewsPlugin) Stop(context.Context) error  { return nil }

func help(tl func(string, string) string) string {
	var cats []string
	for _, c := range categories {
		cats = append(cats, plugin.Code(tl(c[0], c[1])))
	}
	return tl(
		"🗞️ **每日新闻**\n\n"+plugin.Code("news")+" 今日新闻和每日一览\n"+plugin.Code("news 国际")+" 只看某个类目\n\n类目 "+strings.Join(cats, " ")+"\n条数和要显示的栏目在机器人面板里设置\n\n💡 数据来源 news.topurl.cn",
		"🗞️ **Daily news**\n\n"+plugin.Code("news")+" today's news and daily picks (Chinese)\n"+plugin.Code("news world")+" one category only\n\nCategories "+strings.Join(cats, " ")+"\nItem count and sections are set in the bot panel\n\n💡 Source: news.topurl.cn")
}

// parseCategory maps a Chinese or English category name to the API value.
func parseCategory(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, c := range categories {
		if s == c[0] || s == c[1] {
			return c[0], true
		}
	}
	return "", false
}

// categoryLabel shows the API category in the reader's language.
func categoryLabel(tl func(string, string) string, cat string) string {
	for _, c := range categories {
		if c[0] == cat {
			return tl(c[0], c[1])
		}
	}
	return cat
}

func (p *NewsPlugin) handle(ctx *plugin.CommandContext) error {
	arg := ctx.GetArg(0)
	cat := ""
	if arg != "" {
		c, ok := parseCategory(arg)
		if !ok {
			if a := strings.ToLower(arg); a != "help" && a != "h" {
				return ctx.Edit("❌ " + ctx.Tlocal("没有这个类目：", "Unknown category: ") + plugin.Code(arg) + "\n\n" + help(ctx.Tlocal))
			}
			return ctx.Edit(help(ctx.Tlocal))
		}
		cat = c
	}
	_ = ctx.Edit("📰 " + ctx.Tlocal("正在获取今日新闻…", "Fetching today's news…"))
	d, err := p.fetchCached(ctx.Context(), p.set.Int("count"), cat)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("获取失败：", "Fetch failed: ") + plugin.Escape(err.Error()))
	}
	sec := sections{History: p.set.Bool("history"), Phrase: p.set.Bool("phrase"), Sentence: p.set.Bool("sentence"), Poem: p.set.Bool("poem")}
	blocks := render(ctx.Tlocal, d, cat, sec)
	if len(blocks) == 0 {
		return ctx.Edit("📭 " + ctx.Tlocal("今天没有拿到内容", "Nothing came back today"))
	}
	parts := pack(blocks, maxLen)
	if err := edit(ctx, parts[0]); err != nil {
		return err
	}
	for _, s := range parts[1:] {
		if err := send(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

type newsData struct {
	Calendar struct {
		CYear   int    `json:"cYear"`
		CMonth  int    `json:"cMonth"`
		CDay    int    `json:"cDay"`
		NcWeek  string `json:"ncWeek"`
		GzYear  string `json:"gzYear"`
		Animal  string `json:"animal"`
		MonthCn string `json:"monthCn"`
		DayCn   string `json:"dayCn"`
		Term    string `json:"term"`
	} `json:"calendar"`
	NewsList []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"newsList"`
	HistoryList []struct {
		Event string `json:"event"`
	} `json:"historyList"`
	Phrase *struct {
		Phrase  string `json:"phrase"`
		Pinyin  string `json:"pinyin"`
		Explain string `json:"explain"`
	} `json:"phrase"`
	Sentence *struct {
		Sentence string `json:"sentence"`
		Author   string `json:"author"`
	} `json:"sentence"`
	Poem *struct {
		Content []string `json:"content"`
		Title   string   `json:"title"`
		Author  string   `json:"author"`
	} `json:"poem"`
}

func (p *NewsPlugin) fetch(ctx context.Context, count int, cat string) (*newsData, error) {
	q := url.Values{}
	if count >= 8 && count <= 20 {
		q.Set("count", fmt.Sprint(count))
	}
	if cat != "" {
		q.Set("category", cat)
	}
	u := apiURL
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "PaperValet-News/1.0")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data *newsData `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, err
	}
	if body.Data == nil {
		return nil, fmt.Errorf("empty response")
	}
	return body.Data, nil
}

type sections struct{ History, Phrase, Sentence, Poem bool }

// render returns message blocks; each block stays in one message.
func render(tl func(string, string) string, d *newsData, cat string, sec sections) []string {
	var blocks []string
	head := "🗞️ **" + tl("每日新闻", "Daily news") + "**"
	if c := d.Calendar; c.CYear > 0 {
		head += fmt.Sprintf(" · %d-%02d-%02d %s", c.CYear, c.CMonth, c.CDay, c.NcWeek)
		lunar := strings.TrimSpace(c.GzYear + c.Animal + "年 " + c.MonthCn + c.DayCn)
		if c.Term != "" {
			lunar += " · " + c.Term
		}
		head += "\n" + plugin.Escape(lunar)
	}
	var news []string
	for _, n := range d.NewsList {
		t := strings.TrimSpace(n.Title)
		if t == "" {
			continue
		}
		line := fmt.Sprintf("%d. ", len(news)+1)
		if strings.HasPrefix(n.URL, "http://") || strings.HasPrefix(n.URL, "https://") {
			line += plugin.Link(t, n.URL)
		} else {
			line += plugin.Escape(t)
		}
		news = append(news, line)
	}
	if len(news) > 0 {
		title := "📮 **" + tl("今日新闻", "News") + "**"
		if cat != "" {
			title += " · " + plugin.Escape(categoryLabel(tl, cat))
		}
		blocks = append(blocks, head+"\n\n"+title+"\n"+strings.Join(news, "\n"))
	} else {
		blocks = append(blocks, head)
	}
	if sec.History {
		var ev []string
		for _, h := range d.HistoryList {
			if e := strings.TrimSpace(h.Event); e != "" {
				ev = append(ev, "> "+plugin.Escape(e))
			}
		}
		if len(ev) > 0 {
			blocks = append(blocks, "🎬 **"+tl("历史上的今天", "On this day")+"**\n"+strings.Join(ev, "\n"))
		}
	}
	if ph := d.Phrase; sec.Phrase && ph != nil && ph.Phrase != "" {
		b := "🧩 **" + tl("天天成语", "Idiom") + "**\n> **" + plugin.Escape(ph.Phrase) + "**"
		if ph.Pinyin != "" {
			b += " " + plugin.Escape(ph.Pinyin)
		}
		if ph.Explain != "" {
			b += "\n> " + plugin.Escape(ph.Explain)
		}
		blocks = append(blocks, b)
	}
	if s := d.Sentence; sec.Sentence && s != nil && s.Sentence != "" {
		b := "🎻 **" + tl("慧语香风", "Quote") + "**\n> " + plugin.Italic(s.Sentence)
		if s.Author != "" {
			b += "\n> —— " + plugin.Escape(s.Author)
		}
		blocks = append(blocks, b)
	}
	if po := d.Poem; sec.Poem && po != nil && len(po.Content) > 0 {
		var lines []string
		for _, l := range po.Content {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, "> "+plugin.Escape(l))
			}
		}
		b := "🎑 **" + tl("诗歌天地", "Poem") + "**\n" + strings.Join(lines, "\n")
		if po.Title != "" || po.Author != "" {
			b += "\n> —— 《" + plugin.Escape(po.Title) + "》" + plugin.Escape(po.Author)
		}
		blocks = append(blocks, b)
	}
	return blocks
}

// pack joins blocks with blank lines into messages of at most max runes.
// A single oversized block is cut at a line boundary.
func pack(blocks []string, max int) []string {
	var out []string
	cur := ""
	flush := func() {
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
	}
	for _, b := range blocks {
		for len([]rune(b)) > max {
			flush()
			lines := strings.Split(b, "\n")
			head, n := []string{}, 0
			for i, l := range lines {
				if n+len([]rune(l))+1 > max && i > 0 {
					b = strings.Join(lines[i:], "\n")
					break
				}
				head = append(head, l)
				n += len([]rune(l)) + 1
			}
			out = append(out, strings.Join(head, "\n"))
			if len(head) == len(lines) {
				b = ""
			}
		}
		if b == "" {
			continue
		}
		if cur != "" && len([]rune(cur))+2+len([]rune(b)) > max {
			flush()
		}
		if cur == "" {
			cur = b
		} else {
			cur += "\n\n" + b
		}
	}
	flush()
	return out
}

func edit(ctx *plugin.CommandContext, text string) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	plain, ents := plugin.ParseMarkdown(text, nil)
	req := &tg.MessagesEditMessageRequest{Peer: peer, ID: ctx.Message.Message.ID, Message: plain, NoWebpage: true}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	_, err = ctx.API.MessagesEditMessage(ctx.Context(), req)
	return err
}

func send(ctx *plugin.CommandContext, text string) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	plain, ents := plugin.ParseMarkdown(text, nil)
	req := &tg.MessagesSendMessageRequest{Peer: peer, Message: plain, RandomID: time.Now().UnixNano(), NoWebpage: true}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	_, err = ctx.API.MessagesSendMessage(ctx.Context(), req)
	return err
}
