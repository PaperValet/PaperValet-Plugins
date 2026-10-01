package main

import (
	"context"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

type IsAlivePlugin struct{}

func New() *IsAlivePlugin { return &IsAlivePlugin{} }

func (p *IsAlivePlugin) Name() string        { return "isalive" }
func (p *IsAlivePlugin) Description() string { return "检测 bot 是否在线" }
func (p *IsAlivePlugin) DescEN() string      { return "Check whether a bot is alive" }

func (p *IsAlivePlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	cmds := []*plugin.Command{
		{
			Name:        "isalive",
			Aliases:     []string{"alive", "活了么", "在吗"},
			Description: "检测 bot 是否在线",
			DescEN:      "Check whether a bot is alive",
			Usage:       "isalive",
			Plugin:      p.Name(),
			Category:    "core",
			OwnerOnly:   false,
			Handler:     p.handleIsAlive,
		},
	}
	for _, cmd := range cmds {
		if err := mgr.RegisterCommand(cmd); err != nil {
			return err
		}
	}
	return nil
}

func (p *IsAlivePlugin) Start(ctx context.Context) error { return nil }
func (p *IsAlivePlugin) Stop(ctx context.Context) error  { return nil }

func (p *IsAlivePlugin) handleIsAlive(ctx *plugin.CommandContext) error {
	return ctx.Edit("✅ **存活检测**\n" +
		"\n" +
		"状态: `在线`\n" +
		"版本: `PaperValet 1.0`\n" +
		"运行时: `gotd/td`\n" +
		"\n" +
		"💡 Bot 运行正常")
}
