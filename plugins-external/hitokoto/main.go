package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	apiURL     = "https://v1.hitokoto.cn/"
	maxRetries = 10 // as in the reference
	retryDelay = time.Second
)

var Metadata = &plugin.PluginMetadata{
	Name:        "hitokoto",
	Description: "随机一言",
	DescEN:      "Random quote (Hitokoto)",
	Version:     "1.1.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type HitokotoPlugin struct {
	http *http.Client
}

func New() *HitokotoPlugin {
	return &HitokotoPlugin{http: &http.Client{Timeout: 10 * time.Second}}
}

func (p *HitokotoPlugin) Name() string        { return "hitokoto" }
func (p *HitokotoPlugin) Description() string { return Metadata.Description }
func (p *HitokotoPlugin) DescEN() string      { return Metadata.DescEN }

func (p *HitokotoPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "hitokoto",
		Description: "获取随机一言，可按一个或多个类型筛选",
		DescEN:      "Random Hitokoto quote, optionally filtered by one or more types",
		Usage:       "hitokoto [类型 a-l …] | hitokoto help",
		UsageEN:     "hitokoto [types a-l …] | hitokoto help",
		Plugin:      p.Name(),
		Category:    "fun",
		Handler:     p.handleHitokoto,
	})
}

func (p *HitokotoPlugin) Start(ctx context.Context) error { return nil }
func (p *HitokotoPlugin) Stop(ctx context.Context) error  { return nil }

var typeNames = map[string][2]string{
	"a": {"动画", "Anime"}, "b": {"漫画", "Comic"}, "c": {"游戏", "Game"},
	"d": {"文学", "Literature"}, "e": {"原创", "Original"}, "f": {"网络", "Internet"},
	"g": {"其他", "Other"}, "h": {"影视", "Film & TV"}, "i": {"诗词", "Poetry"},
	"j": {"网易云", "NetEase Music"}, "k": {"哲学", "Philosophy"}, "l": {"抖机灵", "Witty"},
}

type hitokotoResp struct {
	Hitokoto string  `json:"hitokoto"`
	From     string  `json:"from"`
	FromWho  *string `json:"from_who"`
	Type     string  `json:"type"`
}

// parseTypes keeps valid type letters (deduplicated, in order). Arguments
// such as "a,c" or "ach" are split as well. Unknown arguments are returned
// separately so the user gets feedback instead of silent ignoring.
func parseTypes(args []string) (types, invalid []string) {
	seen := map[string]bool{}
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			types = append(types, t)
		}
	}
	for _, a := range args {
		for _, part := range strings.FieldsFunc(strings.ToLower(a), func(r rune) bool { return r == ',' || r == '，' || r == '|' }) {
			if _, ok := typeNames[part]; ok {
				add(part)
				continue
			}
			// "ach" → a, c, h when every letter is a valid type.
			allValid := len(part) > 1
			for _, r := range part {
				if _, ok := typeNames[string(r)]; !ok {
					allValid = false
					break
				}
			}
			if allValid {
				for _, r := range part {
					add(string(r))
				}
				continue
			}
			invalid = append(invalid, part)
		}
	}
	return types, invalid
}

func (p *HitokotoPlugin) typeList(ctx *plugin.CommandContext) string {
	keys := make([]string, 0, len(typeNames))
	for k := range typeNames {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		n := typeNames[k]
		b.WriteString(plugin.Code(k) + " " + plugin.Escape(ctx.Tlocal(n[0], n[1])))
		if i%3 == 2 {
			b.WriteString("\n")
		} else {
			b.WriteString("  ")
		}
	}
	return strings.TrimRight(b.String(), " \n")
}

func (p *HitokotoPlugin) help(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"💬 **一言**\n\n"+
			"**用法**\n"+
			plugin.Code("hitokoto")+" 随机一言\n"+
			plugin.Code("hitokoto a")+" 只获取动画类\n"+
			plugin.Code("hitokoto a c h")+" 从多个类型中随机（也可写 "+plugin.Code("ach")+"）\n"+
			plugin.Code("hitokoto hh")+" 只获取影视类（单独的 h 为帮助）\n\n"+
			"**类型**\n"+p.typeList(ctx)+"\n\n"+
			"💡 数据来源：hitokoto.cn",
		"💬 **Hitokoto**\n\n"+
			"**Usage**\n"+
			plugin.Code("hitokoto")+" random quote\n"+
			plugin.Code("hitokoto a")+" anime quotes only\n"+
			plugin.Code("hitokoto a c h")+" random among several types (or "+plugin.Code("ach")+")\n"+
			plugin.Code("hitokoto hh")+" film & TV only (a lone h shows help)\n\n"+
			"**Types**\n"+p.typeList(ctx)+"\n\n"+
			"💡 Source: hitokoto.cn")
}

func (p *HitokotoPlugin) handleHitokoto(ctx *plugin.CommandContext) error {
	if len(ctx.Args) > 0 {
		// Like the reference, "help"/"h" as first word shows help; a lone
		// 影视 type can still be requested as "hh" or "h,".
		if a := strings.ToLower(ctx.Args[0]); a == "help" || a == "h" {
			return ctx.Edit(p.help(ctx))
		}
	}
	types, invalid := parseTypes(ctx.Args)
	if len(invalid) > 0 && len(types) == 0 {
		return ctx.Edit("❌ " + ctx.Tlocal("无效类型：", "Invalid type: ") + plugin.Code(strings.Join(invalid, " ")) +
			"\n\n💡 " + ctx.Tlocal("可选类型 a-l，发送 ", "Valid types are a-l, send ") + plugin.Code("hitokoto help"))
	}

	_ = ctx.Edit("🔄 " + ctx.Tlocal("正在获取一言…", "Fetching quote…"))

	data, err := p.fetch(ctx.Context(), types)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("获取一言失败，请稍后重试：", "Failed to fetch quote, retry later: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit(format(ctx, data))
}

func (p *HitokotoPlugin) fetch(ctx context.Context, types []string) (*hitokotoResp, error) {
	q := url.Values{"encode": {"json"}, "charset": {"utf-8"}}
	for _, t := range types {
		q.Add("c", t) // repeated c= picks randomly among the types
	}
	u := apiURL + "?" + q.Encode()
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay):
			}
		}
		data, err := p.once(ctx, u)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

func (p *HitokotoPlugin) once(ctx context.Context, u string) (*hitokotoResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "PaperValet-Hitokoto/1.1")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var data hitokotoResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return nil, err
	}
	if strings.TrimSpace(data.Hitokoto) == "" {
		return nil, errors.New("empty response")
	}
	return &data, nil
}

func format(ctx *plugin.CommandContext, d *hitokotoResp) string {
	var src strings.Builder
	if d.From != "" {
		src.WriteString("《" + plugin.Escape(d.From) + "》")
	}
	if n, ok := typeNames[d.Type]; ok {
		src.WriteString(ctx.Tlocal("（"+n[0]+"）", " ("+n[1]+")"))
	}
	if d.FromWho != nil && *d.FromWho != "" && *d.FromWho != "null" {
		src.WriteString(" - " + plugin.Escape(*d.FromWho))
	}
	out := "💬 " + plugin.Escape(d.Hitokoto)
	if src.Len() > 0 {
		out += "\n\n📚 " + src.String()
	}
	return out
}
