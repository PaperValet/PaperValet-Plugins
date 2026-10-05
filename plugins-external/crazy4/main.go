package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"math/rand/v2"
	"strings"
	"sync"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

//go:embed texts.json
var textsJSON []byte

var Metadata = &plugin.PluginMetadata{
	Name:        "crazy4",
	Description: "随机疯狂星期四文案",
	DescEN:      "Random KFC Crazy Thursday copypasta",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type Crazy4Plugin struct {
	host plugin.Host
	deck *deck
}

func New() *Crazy4Plugin { return &Crazy4Plugin{} }

func (p *Crazy4Plugin) Name() string        { return "crazy4" }
func (p *Crazy4Plugin) Description() string { return Metadata.Description }
func (p *Crazy4Plugin) DescEN() string      { return Metadata.DescEN }

func (p *Crazy4Plugin) Init(_ context.Context, mgr plugin.Manager) error {
	var texts []string
	if err := json.Unmarshal(textsJSON, &texts); err != nil {
		return err
	}
	p.host = mgr.Host()
	p.deck = newDeck(texts)
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "crazy4",
		Description: "发一条随机疯狂星期四文案，替换掉命令消息",
		DescEN:      "Post a random Crazy Thursday copypasta in place of the command",
		Usage:       "crazy4（可回复某条消息）",
		UsageEN:     "crazy4 (optionally as a reply)",
		Plugin:      p.Name(),
		Category:    "fun",
		Handler:     p.handle,
	})
}

func (p *Crazy4Plugin) Start(context.Context) error { return nil }
func (p *Crazy4Plugin) Stop(context.Context) error  { return nil }

func (p *Crazy4Plugin) handle(ctx *plugin.CommandContext) error {
	if a := strings.ToLower(ctx.GetArg(0)); a == "help" || a == "h" {
		return ctx.Edit(ctx.Tlocal(
			"🍗 **疯狂星期四**\n\n"+plugin.Code("crazy4")+" 随机发一条文案，命令消息会被删掉\n回复别人时发，文案也回复那条消息",
			"🍗 **Crazy Thursday**\n\n"+plugin.Code("crazy4")+" posts a random copypasta (Chinese) and deletes the command\nSend it as a reply and the copypasta replies to the same message"))
	}
	replyTo := realReplyID(ctx.Message.Message)
	if _, err := p.host.Send(ctx.Context(), ctx.Message.ChatID, plugin.Escape(p.deck.next()), replyTo); err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("发送失败：", "Send failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Delete()
}

// realReplyID returns the replied message id, ignoring the implicit
// reply-to-topic-root header every forum topic message carries (that
// header is not a real reply; the copypasta would otherwise attach to the
// topic's creation message).
func realReplyID(msg *tg.Message) int {
	if msg == nil {
		return 0
	}
	h, ok := msg.ReplyTo.(*tg.MessageReplyHeader)
	if !ok || h.ReplyToMsgID == 0 {
		return 0
	}
	if h.ForumTopic {
		if _, has := h.GetReplyToTopID(); !has {
			return 0
		}
	}
	return h.ReplyToMsgID
}

// deck hands out texts in shuffled order so nothing repeats until every
// text has been used once.
type deck struct {
	mu    sync.Mutex
	texts []string
	order []int
	last  int
}

func newDeck(texts []string) *deck { return &deck{texts: texts, last: -1} }

func (d *deck) next() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.order) == 0 {
		d.order = rand.Perm(len(d.texts))
		// Avoid the same text twice in a row across a reshuffle.
		if len(d.order) > 1 && d.order[len(d.order)-1] == d.last {
			d.order[0], d.order[len(d.order)-1] = d.order[len(d.order)-1], d.order[0]
		}
	}
	i := d.order[len(d.order)-1]
	d.order = d.order[:len(d.order)-1]
	d.last = i
	return d.texts[i]
}
