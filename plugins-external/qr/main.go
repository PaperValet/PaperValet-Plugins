package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
	"github.com/skip2/go-qrcode"
)

type QRPlugin struct{}

func New() *QRPlugin { return &QRPlugin{} }

func (p *QRPlugin) Name() string        { return "qr" }
func (p *QRPlugin) Description() string { return "二维码生成" }
func (p *QRPlugin) DescEN() string      { return "QR code generator" }

func (p *QRPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "qr",
		Aliases:     []string{"qrcode", "二维码"},
		Description: "生成二维码图片",
		Usage:       "qr <内容>",
		Plugin:      p.Name(),
		Category:    "tools",
		Handler:     p.handleQR,
	})
}

func (p *QRPlugin) Start(ctx context.Context) error { return nil }
func (p *QRPlugin) Stop(ctx context.Context) error  { return nil }

func (p *QRPlugin) handleQR(ctx *plugin.CommandContext) error {
	if len(ctx.Args) == 0 {
		return ctx.Edit("📱 用法：`qr <内容>`\n示例：`qr https://github.com`")
	}
	if ctx.Media == nil {
		return ctx.Edit("❌ 媒体服务未就绪")
	}
	content := strings.Join(ctx.Args, " ")

	png, err := qrcode.Encode(content, qrcode.Medium, 512)
	if err != nil {
		return ctx.Edit("❌ 二维码生成失败: " + plugin.Escape(err.Error()))
	}

	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("pv_qr_%d.png", ctx.Message.Message.ID))
	if err := os.WriteFile(tmp, png, 0o600); err != nil {
		return ctx.Edit("❌ 写入临时文件失败: " + plugin.Escape(err.Error()))
	}
	defer os.Remove(tmp)

	if err := ctx.ReplyMedia(tmp, content); err != nil {
		return ctx.Edit("❌ 发送二维码失败: " + plugin.Escape(err.Error()))
	}
	return ctx.Delete()
}
