package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	apiURL   = "https://api.oddfar.com/yl/q.php?c=1009&encode=text"
	attempts = 5
)

var Metadata = &plugin.PluginMetadata{
	Name:        "diss",
	Description: "儒雅随和版祖安语录",
	DescEN:      "Random polite roast",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type DissPlugin struct{ http *http.Client }

func New() *DissPlugin { return &DissPlugin{http: &http.Client{Timeout: 10 * time.Second}} }

func (p *DissPlugin) Name() string        { return "diss" }
func (p *DissPlugin) Description() string { return Metadata.Description }
func (p *DissPlugin) DescEN() string      { return Metadata.DescEN }

func (p *DissPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "diss",
		Description: "随机一句儒雅随和的祖安语录",
		DescEN:      "Send a random polite roast (Chinese)",
		Usage:       "diss",
		UsageEN:     "diss",
		Plugin:      p.Name(),
		Category:    "fun",
		Handler:     p.handle,
	})
}

func (p *DissPlugin) Start(context.Context) error { return nil }
func (p *DissPlugin) Stop(context.Context) error  { return nil }

func (p *DissPlugin) handle(ctx *plugin.CommandContext) error {
	if a := strings.ToLower(ctx.GetArg(0)); a == "help" || a == "h" {
		return ctx.Edit(ctx.Tlocal(
			"🗣️ **儒雅随和**\n\n"+plugin.Code("diss")+" 随机一句祖安语录",
			"🗣️ **Diss**\n\n"+plugin.Code("diss")+" a random polite roast (Chinese)"))
	}
	_ = ctx.Edit("🔄 " + ctx.Tlocal("正在获取儒雅随和语录…", "Fetching a roast…"))
	text, err := p.fetch(ctx.Context())
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("试了好多次都连不上 API：", "API unreachable after several tries: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit(plugin.Escape(text))
}

func (p *DissPlugin) fetch(ctx context.Context) (string, error) {
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Second):
			}
		}
		s, err := p.once(ctx)
		if err == nil {
			return s, nil
		}
		last = err
	}
	return "", last
}

func (p *DissPlugin) once(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	resp, err := p.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	return clean(string(b))
}

// clean trims the API body and rejects empty or HTML error pages.
func clean(s string) (string, error) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "\ufeff"))
	if s == "" {
		return "", errors.New("empty response")
	}
	if strings.HasPrefix(s, "<") {
		return "", errors.New("unexpected HTML response")
	}
	return s, nil
}
