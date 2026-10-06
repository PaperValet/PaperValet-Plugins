// sticker.go implements "yvlu s": save a replied sticker/photo into the
// configured sticker pack, creating the pack when it does not exist.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const stickerEmoji = "📝"

// configuredPack reads the pack short name from the settings panel.
func (p *YvluPlugin) configuredPack(ctx *plugin.CommandContext) string {
	if p.settings == nil {
		return ""
	}
	return strings.TrimSpace(p.settings.String("stickerSet"))
}

// handleSave implements "yvlu s" on a replied sticker or photo message.
func (p *YvluPlugin) handleSave(ctx *plugin.CommandContext) error {
	if !ctx.Message.IsReply || ctx.Message.ReplyToID == 0 {
		return ctx.Edit("❌ " + ctx.Tlocal("请回复一张贴纸或图片", "Reply to a sticker or photo first"))
	}
	pack := p.configuredPack(ctx)
	if pack == "" {
		return ctx.Edit("❌ " + ctx.Tlocal(
			"未配置贴纸包：请在机器人面板的 yvlu 设置里填写贴纸包名称",
			"No sticker pack configured: set the pack name in the yvlu panel of the bot"))
	}

	reply, err := ctx.ReplyMessage()
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("获取回复消息失败: ", "Failed to load the replied message: ") +
			plugin.Escape(describeErr(err)))
	}

	var (
		doc      *tg.Document
		photoLoc tg.InputFileLocationClass
	)
	switch m := reply.Media.(type) {
	case *tg.MessageMediaDocument:
		if d, ok := m.Document.(*tg.Document); ok {
			if _, isSticker := docSticker(d); isSticker {
				doc = d
			}
		}
	case *tg.MessageMediaPhoto:
		if photo, ok := m.Photo.(*tg.Photo); ok {
			typ, _, _, _ := largestPhotoSize(photo.Sizes)
			if typ != "" {
				photoLoc = &tg.InputPhotoFileLocation{
					ID: photo.ID, AccessHash: photo.AccessHash,
					FileReference: photo.FileReference, ThumbSize: typ,
				}
			}
		}
	}
	if doc == nil && photoLoc == nil {
		return ctx.Edit("❌ " + ctx.Tlocal("回复的消息不是贴纸或图片", "The replied message is not a sticker or photo"))
	}

	_ = ctx.Edit("🔄 " + ctx.Tlocal("正在处理...", "Processing..."))

	// Prepare the sticker document: stickers are reused via inputDocument;
	// photos are converted to a 512-side WebP (Telegram's static sticker
	// dimension rule) before uploading.
	var inputDoc tg.InputDocumentClass
	switch {
	case doc != nil:
		inputDoc = &tg.InputDocument{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}
	default:
		src, err := p.tempDownload(ctx, photoLoc)
		if err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("下载图片失败: ", "Failed to download the photo: ") + plugin.Escape(describeErr(err)))
		}
		defer os.Remove(src)
		stickerPath, err := p.convertToStickerWebP(ctx.Context(), src)
		if err != nil {
			os.Remove(stickerPath)
			return ctx.Edit("❌ " + ctx.Tlocal("图片转贴纸失败（需要 ffmpeg）: ", "Photo→sticker conversion failed (ffmpeg required): ") + plugin.Escape(describeErr(err)))
		}
		defer os.Remove(stickerPath)
		docUploaded, err := uploadStickerDoc(ctx, stickerPath, "image/webp")
		if err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("上传贴纸失败: ", "Failed to upload the sticker: ") + plugin.Escape(describeErr(err)))
		}
		inputDoc = docUploaded
	}

	setRef := &tg.InputStickerSetShortName{ShortName: pack}
	item := tg.InputStickerSetItem{Document: inputDoc, Emoji: stickerEmoji}

	// Existing pack: append. Missing pack: create with this sticker first.
	if _, exists, err := lookupStickerSet(ctx, pack); err != nil {
		return ctx.Edit("❌ " + plugin.Escape(describeErr(err)))
	} else if exists {
		if _, err := ctx.API.StickersAddStickerToSet(ctx.Context(), &tg.StickersAddStickerToSetRequest{
			Stickerset: setRef, Sticker: item,
		}); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("添加贴纸失败: ", "Failed to add the sticker: ") +
				plugin.Escape(friendlyStickerErr(ctx, err)))
		}
	} else {
		if _, err := ctx.API.StickersCreateStickerSet(ctx.Context(), &tg.StickersCreateStickerSetRequest{
			UserID:    &tg.InputUserSelf{},
			Title:     pack,
			ShortName: pack,
			Stickers:  []tg.InputStickerSetItem{item},
		}); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("创建贴纸包失败: ", "Failed to create the sticker pack: ") +
				plugin.Escape(friendlyStickerErr(ctx, err)))
		}
	}

	link := plugin.Link(pack, "https://t.me/addstickers/"+pack)
	return ctx.Edit("✅ " + ctx.Tlocal(
		fmt.Sprintf("已保存到贴纸包 %s", link),
		fmt.Sprintf("Saved to the pack %s", link)))
}

// convertToStickerWebP pads src onto a 512×512 transparent canvas and
// re-encodes as WebP — Telegram rejects static stickers whose sides are not
// 512×N / N×512 (STICKER_PNG_DIMENSIONS), so a raw photo upload always fails.
// Returns the output path; the file is the caller's to remove.
func (p *YvluPlugin) convertToStickerWebP(ctx context.Context, src string) (string, error) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", errors.New("ffmpeg not found")
	}
	out := filepath.Join(p.tmp(), fmt.Sprintf("sticker_%d.webp", time.Now().UnixNano()))
	c, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", src,
		"-vf", "scale=512:512:force_original_aspect_ratio=decrease," +
			"pad=512:512:(ow-iw)/2:(oh-ih)/2:color=black@0,format=yuva420p",
		"-c:v", "libwebp", "-lossless", "0", "-q:v", "90", "-frames:v", "1",
		out,
	}
	if outb, err := exec.CommandContext(c, bin, args...).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(outb))
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		if msg == "" {
			msg = err.Error()
		}
		if c.Err() != nil {
			msg = "timeout"
		}
		os.Remove(out)
		return "", fmt.Errorf("ffmpeg: %s", msg)
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		os.Remove(out)
		return "", errors.New("ffmpeg produced no output")
	}
	return out, nil
}

// tempDownload downloads a file location into a fresh temp file.
func (p *YvluPlugin) tempDownload(ctx *plugin.CommandContext, loc tg.InputFileLocationClass) (string, error) {
	if err := os.MkdirAll(p.tmp(), 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(p.tmp(), fmt.Sprintf("save_%d.img", time.Now().UnixNano()))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := downloader.NewDownloader().Download(ctx.API, loc).Stream(ctx.Context(), f); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// lookupStickerSet fetches a sticker set by short name; ok=false means it
// does not exist (STICKERSET_INVALID).
func lookupStickerSet(ctx *plugin.CommandContext, name string) (*tg.MessagesStickerSet, bool, error) {
	res, err := ctx.API.MessagesGetStickerSet(ctx.Context(), &tg.MessagesGetStickerSetRequest{
		Stickerset: &tg.InputStickerSetShortName{ShortName: name},
		Hash:       0,
	})
	if err != nil {
		if tgerr.Is(err, "STICKERSET_INVALID") {
			return nil, false, nil
		}
		return nil, false, err
	}
	set, ok := res.(*tg.MessagesStickerSet)
	if !ok {
		return nil, true, nil // not modified: treat as existing
	}
	return set, true, nil
}

// friendlyStickerErr maps known sticker API errors to bilingual text.
func friendlyStickerErr(ctx *plugin.CommandContext, err error) string {
	switch {
	case tgerr.Is(err, "STICKERSET_INVALID"):
		return ctx.Tlocal("贴纸包名称无效或已被占用", "Pack name invalid or taken")
	case tgerr.Is(err, "STICKER_PNG_DIMENSIONS"):
		return ctx.Tlocal("静态贴纸尺寸必须为 512xN 或 Nx512", "Static stickers must be 512xN or Nx512")
	case tgerr.Is(err, "STICKER_VIDEO_LONG"):
		return ctx.Tlocal("视频贴纸时长不能超过 3 秒", "Video stickers must be at most 3 seconds")
	case tgerr.Is(err, "STICKERS_TOO_MUCH"):
		return ctx.Tlocal("贴纸包已满", "The sticker pack is full")
	case tgerr.Is(err, "STICKER_EMOJI_INVALID"):
		return ctx.Tlocal("贴纸 emoji 无效", "Invalid sticker emoji")
	}
	if d, ok := tgerr.AsFloodWait(err); ok {
		_ = d
		return describeErr(err)
	}
	return describeErr(err)
}
