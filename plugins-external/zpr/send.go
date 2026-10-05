// Sending artwork photos for the zpr plugin: one photo per message with a
// caption (spoilered for R18), replying to the same message the command
// replied to — ported from the TeleBox zpr plugin.

package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"mime"
	"path/filepath"
	"strings"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	captionMaxRunes = 900 // Telegram caps captions at 1024; leave headroom
	tagMaxRunes     = 160
)

// sendAll sends every file as its own photo message, like the source's
// per-item loop. The reply target is the message the command itself
// replied to (msg.replyToMessage), matching the source's replyTo.
func (p *ZprPlugin) sendAll(ctx *plugin.CommandContext, files []imageFile, r18, withCaption bool) error {
	replyTo := 0
	if ctx.Message != nil {
		replyTo = ctx.Message.ReplyToID
	}
	for _, f := range files {
		caption := ""
		if withCaption {
			caption = buildCaption(f.Item, ctx.Lang)
		}
		if err := p.sendMedia(ctx, f, caption, r18, replyTo); err != nil {
			if ctx.Logger != nil {
				ctx.Logger.Warn("zpr: send failed", "pid", f.Item.PID, "err", err)
			}
			// The source aborts the loop on the first failure; a chat that
			// rejects media rejects the rest too.
			return err
		}
	}
	return nil
}

// sendMedia uploads one file as a photo (spoilered for r18) and falls
// back to a document when Telegram rejects the photo payload.
func (p *ZprPlugin) sendMedia(ctx *plugin.CommandContext, f imageFile, caption string, spoiler bool, replyTo int) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	plain, entities := plugin.ParseMarkdown(caption, nil)
	if len([]rune(plain)) > 1024 {
		plain, entities = "", nil
	}
	file, err := uploader.NewUploader(ctx.API).FromPath(ctx.Context(), f.Path)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	photo := &tg.InputMediaUploadedPhoto{File: file}
	if spoiler {
		photo.SetSpoiler(true)
	}
	req := &tg.MessagesSendMediaRequest{
		Peer:     peer,
		Media:    photo,
		Message:  plain,
		RandomID: rand.Int64(),
	}
	if len(entities) > 0 {
		req.SetEntities(entities)
	}
	if replyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	_, err = ctx.API.MessagesSendMedia(ctx.Context(), req)
	if err != nil && isPhotoReject(err) {
		if ctx.Logger != nil {
			ctx.Logger.Warn("zpr: photo rejected, retrying as document", "err", err)
		}
		return p.sendDocument(ctx, f, plain, entities, spoiler, replyTo)
	}
	return err
}

// sendDocument uploads the file as a document (spoiler still supported),
// the fallback for photos Telegram refuses.
func (p *ZprPlugin) sendDocument(ctx *plugin.CommandContext, f imageFile, caption string, entities []tg.MessageEntityClass, spoiler bool, replyTo int) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	file, err := uploader.NewUploader(ctx.API).FromPath(ctx.Context(), f.Path)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	mt := mime.TypeByExtension(filepath.Ext(f.Path))
	if mt == "" || !strings.HasPrefix(mt, "image/") {
		mt = "image/jpeg"
	}
	doc := &tg.InputMediaUploadedDocument{
		File:     file,
		MimeType: mt,
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: filepath.Base(f.Path)},
		},
	}
	if spoiler {
		doc.SetSpoiler(true)
	}
	req := &tg.MessagesSendMediaRequest{
		Peer:     peer,
		Media:    doc,
		Message:  caption,
		RandomID: rand.Int64(),
	}
	if len(entities) > 0 {
		req.SetEntities(entities)
	}
	if replyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	_, err = ctx.API.MessagesSendMedia(ctx.Context(), req)
	return err
}

// buildCaption renders the per-image caption: title, pid link, author and
// truncated tags, mirroring the source's caption fields.
func buildCaption(it setuItem, lang string) string {
	zh := lang != "en-US"
	var b strings.Builder
	title := strings.TrimSpace(it.Title)
	if title == "" {
		if zh {
			title = "无题"
		} else {
			title = "Untitled"
		}
	}
	b.WriteString("🎨 " + plugin.Bold(title) + "\n")
	link := fmt.Sprintf("https://www.pixiv.net/artworks/%d", it.PID)
	if zh {
		b.WriteString("🆔 作品：" + plugin.Link(fmt.Sprint(it.PID), link) + "\n")
		if a := strings.TrimSpace(it.Author); a != "" {
			b.WriteString("👤 画师：" + plugin.Escape(a) + "\n")
		}
		if tags := joinTags(it.Tags); tags != "" {
			b.WriteString("🏷 标签：" + plugin.Escape(tags))
		}
	} else {
		b.WriteString("🆔 Artwork: " + plugin.Link(fmt.Sprint(it.PID), link) + "\n")
		if a := strings.TrimSpace(it.Author); a != "" {
			b.WriteString("👤 Artist: " + plugin.Escape(a) + "\n")
		}
		if tags := joinTags(it.Tags); tags != "" {
			b.WriteString("🏷 Tags: " + plugin.Escape(tags))
		}
	}
	return truncateRunes(strings.TrimRight(b.String(), "\n"), captionMaxRunes)
}

// joinTags flattens the tag list, truncated to tagMaxRunes runes.
func joinTags(tags []string) string {
	var parts []string
	n := 0
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		rlen := len([]rune(t))
		if n > 0 && n+2+rlen > tagMaxRunes {
			break
		}
		if rlen > tagMaxRunes {
			t = truncateRunes(t, tagMaxRunes)
			rlen = len([]rune(t))
		}
		parts = append(parts, t)
		n += rlen + 2
	}
	return strings.Join(parts, ", ")
}

// truncateRunes keeps at most max runes, marking truncation.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// isPhotoReject reports whether Telegram rejected the photo payload
// itself, so a document send may still work.
func isPhotoReject(err error) bool {
	var gerr *tgerr.Error
	if errors.As(err, &gerr) {
		switch gerr.Type {
		case "PHOTO_INVALID_DIMENSIONS", "PHOTO_INVALID_EXTENT",
			"PHOTO_SAVE_FILE_INVALID", "PHOTO_INVALID", "FILE_PARTS_INVALID":
			return true
		}
	}
	return false
}

// describeSendErr turns a Telegram send error into a short readable
// message, calling out FLOOD_WAIT and media-forbidden chats.
func describeSendErr(err error, ctx *plugin.CommandContext) string {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf("FLOOD_WAIT %d s", int(d.Seconds()))
	}
	var gerr *tgerr.Error
	if errors.As(err, &gerr) {
		if gerr.Type == "CHAT_SEND_MEDIA_FORBIDDEN" {
			return ctx.Tlocal("此聊天不允许发送媒体", "This chat forbids sending media")
		}
		return gerr.Type + ": " + gerr.Message
	}
	return trimErr(err.Error())
}
