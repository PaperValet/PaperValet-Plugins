package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	unitDelay     = 300 * time.Millisecond
	maxCaptionLen = 1024
	maxFloodWait  = 120 * time.Second
)

var errNoContent = errors.New("message has no content to save")

// unit is one message or album saved together.
type unit struct {
	info *chatInfo
	msgs []*tg.Message
}

func (u unit) ids() []int {
	ids := make([]int, len(u.msgs))
	for i, m := range u.msgs {
		ids[i] = m.ID
	}
	return ids
}

// srcRef identifies a saved source message.
type srcRef struct {
	info  *chatInfo
	msgID int
}

// job holds the state of one save command.
type job struct {
	p       *SavePlugin
	ctx     *plugin.CommandContext
	jctx    context.Context
	api     *tg.Client
	dest    tg.InputPeerClass
	label   string
	local   bool
	lastRun time.Time
	lastTxt string

	ok, skipped, failed int
	failures            []string
	lastSent            int
	sources             []srcRef
	files               []*localFile
}

func (j *job) tl(zh, en string) string { return j.ctx.Tlocal(zh, en) }

// progress edits the command message, throttled unless force is set.
func (j *job) progress(text string, force bool) {
	if text == j.lastTxt || (!force && time.Since(j.lastRun) < editInterval) {
		return
	}
	j.lastRun, j.lastTxt = time.Now(), text
	_ = j.ctx.Edit(text)
}

func (j *job) fail(what string, err error) {
	j.failed++
	if len(j.failures) < 10 {
		j.failures = append(j.failures, what+": "+errText(err))
	}
}

func errText(err error) string {
	if e, ok := tgerr.As(err); ok {
		return e.Type
	}
	s := err.Error()
	if len([]rune(s)) > 120 {
		s = string([]rune(s)[:120]) + "…"
	}
	return s
}

// call runs f, waiting out short FLOOD_WAITs.
func (j *job) call(f func() error) error {
	for attempt := 0; ; attempt++ {
		err := f()
		d, ok := tgerr.AsFloodWait(err)
		if !ok || attempt >= 2 || d > maxFloodWait {
			return err
		}
		j.progress(fmt.Sprintf("⏳ "+j.tl("触发频率限制，等待 %d 秒…", "Flood wait, sleeping %ds…"), int(d.Seconds())+1), true)
		if serr := sleep(j.jctx, d+time.Second); serr != nil {
			return serr
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// cmdSave implements reply / link / range saving.
func (p *SavePlugin) cmdSave(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	cfg := p.userConfig(ctx.Message.UserID)
	pa := classifyArgs(ctx.Args)
	if pa.BadLink != "" {
		return ctx.Edit("❌ " + tl("无效的消息链接 ", "Invalid message link ") + plugin.Code(pa.BadLink) + "\n" + plugin.Escape(pa.BadReason))
	}
	rChat, rMsg, isReply := replyTarget(ctx.Message)
	target := cfg.Target
	if pa.TempTarget != "" {
		target = pa.TempTarget
	} else if len(pa.Links) == 0 && pa.Range == nil && isReply && len(ctx.Args) > 0 {
		target = strings.Join(ctx.Args, " ")
	}
	if len(pa.Links) == 0 && pa.Range == nil {
		if !isReply {
			if len(ctx.Args) > 0 {
				return ctx.Edit("❌ " + tl("未识别的参数 ", "Unknown argument ") + plugin.Code(ctx.Args[0]) + "\n\n💡 " + plugin.Code("save help"))
			}
			return ctx.Edit(helpText(ctx))
		}
	}
	if len(pa.Links) > maxLinks {
		return ctx.Edit(fmt.Sprintf("❌ "+tl("一次最多 %d 个链接", "At most %d links at once"), maxLinks))
	}
	t, err := normalizeTarget(target)
	if err != nil {
		return ctx.Edit("❌ " + tl("目标无效 ", "Invalid target ") + plugin.Code(target))
	}

	jctx, done, err := p.beginJob(ctx.Context())
	if err != nil {
		return ctx.Edit("❌ " + tl("插件正在停止", "Plugin is stopping"))
	}
	defer done()
	j := &job{p: p, ctx: ctx, jctx: jctx, api: ctx.API, local: t == "local"}
	if !j.local {
		j.dest, j.label, err = resolveTarget(ctx, t)
		if err != nil {
			return ctx.Edit("❌ " + tl("无法访问目标对话 ", "Cannot access target chat ") + plugin.Code(t) + "\n" + plugin.Escape(errText(err)))
		}
	}

	switch {
	case pa.Range != nil:
		err = j.runRange(pa.Range[0], pa.Range[1])
	case len(pa.Links) > 0:
		err = j.runLinks(pa.Links)
	default:
		err = j.runReply(rChat, rMsg)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return ctx.Edit("❌ " + plugin.Escape(errText(err)))
	}
	if cfg.ShowSource && !j.local && j.lastSent > 0 && len(j.sources) > 0 {
		j.sendSourceInfo(pa.Range != nil)
	}
	return ctx.Edit(j.summary())
}

func (j *job) peerFor(chatID int64) (*chatInfo, error) {
	if j.ctx.PeerResolver == nil {
		return nil, errors.New("no peer resolver")
	}
	if chatID == j.ctx.SelfID && chatID != 0 {
		return &chatInfo{Peer: &tg.InputPeerSelf{}, ChatID: chatID}, nil
	}
	peer, err := j.ctx.PeerResolver.ResolveFromChatID(j.jctx, chatID)
	if err != nil {
		return nil, err
	}
	return &chatInfo{Peer: peer, ChatID: chatID}, nil
}

func (j *job) linkChat(l msgLink, cache map[string]*chatInfo) (*chatInfo, error) {
	key := strings.ToLower(l.Username) + "/" + strconv.FormatInt(l.ChannelID, 10)
	if c, ok := cache[key]; ok {
		return c, nil
	}
	var info *chatInfo
	if l.Username != "" {
		if j.ctx.PeerResolver == nil {
			return nil, errors.New("no peer resolver")
		}
		peer, err := j.ctx.PeerResolver.ResolveUsername(j.jctx, l.Username)
		if err != nil {
			return nil, err
		}
		info = &chatInfo{Peer: peer, ChatID: chatIDOf(peer), Username: l.Username}
	} else {
		var err error
		if info, err = j.peerFor(channelChatID(l.ChannelID)); err != nil {
			return nil, err
		}
	}
	cache[key] = info
	return info, nil
}

func (j *job) runReply(chatID int64, msgID int) error {
	j.progress("🔍 "+j.tl("获取消息…", "Fetching message…"), true)
	info, err := j.peerFor(chatID)
	if err != nil {
		return err
	}
	var msgs map[int]*tg.Message
	if err := j.call(func() (e error) { msgs, e = fetchMessages(j.jctx, j.api, info, []int{msgID}); return }); err != nil {
		return err
	}
	m, ok := msgs[msgID]
	if !ok {
		return errors.New(j.tl("无法获取被回复的消息", "Cannot fetch the replied message"))
	}
	album, err := fetchAlbum(j.jctx, j.api, info, m)
	if err != nil {
		album = []*tg.Message{m}
	}
	j.processUnit(unit{info: info, msgs: album}, "")
	return nil
}

func (j *job) runLinks(links []msgLink) error {
	cache := map[string]*chatInfo{}
	var units []unit
	seenGroup := map[string]bool{}
	seenMsg := map[string]bool{}
	for i, l := range links {
		j.progress(fmt.Sprintf("🔍 "+j.tl("获取消息 %d/%d…", "Fetching message %d/%d…"), i+1, len(links)), false)
		info, err := j.linkChat(l, cache)
		if err != nil {
			j.fail(l.String(), err)
			continue
		}
		var msgs map[int]*tg.Message
		if err := j.call(func() (e error) { msgs, e = fetchMessages(j.jctx, j.api, info, []int{l.MsgID}); return }); err != nil {
			if j.jctx.Err() != nil {
				return j.jctx.Err()
			}
			j.fail(l.String(), err)
			continue
		}
		m, ok := msgs[l.MsgID]
		if !ok {
			j.fail(l.String(), errors.New(j.tl("消息不存在或无法访问", "message not found or inaccessible")))
			continue
		}
		key := fmt.Sprintf("%d/%d", info.ChatID, m.ID)
		if seenMsg[key] {
			continue
		}
		if gid, ok := m.GetGroupedID(); ok && gid != 0 {
			gk := fmt.Sprintf("%d/%d", info.ChatID, gid)
			if seenGroup[gk] {
				continue
			}
			seenGroup[gk] = true
			album, err := fetchAlbum(j.jctx, j.api, info, m)
			if err != nil {
				album = []*tg.Message{m}
			}
			for _, a := range album {
				seenMsg[fmt.Sprintf("%d/%d", info.ChatID, a.ID)] = true
			}
			units = append(units, unit{info: info, msgs: album})
			continue
		}
		seenMsg[key] = true
		units = append(units, unit{info: info, msgs: []*tg.Message{m}})
	}
	for i, u := range units {
		if j.jctx.Err() != nil {
			return j.jctx.Err()
		}
		prefix := ""
		if len(units) > 1 {
			prefix = fmt.Sprintf("[%d/%d] ", i+1, len(units))
		}
		j.processUnit(u, prefix)
		if i < len(units)-1 {
			if err := sleep(j.jctx, unitDelay); err != nil {
				return err
			}
		}
	}
	return nil
}

func (j *job) runRange(a, b msgLink) error {
	lo, hi := a.MsgID, b.MsgID
	if lo > hi {
		lo, hi = hi, lo
	}
	if hi-lo+1 > maxRange {
		return fmt.Errorf(j.tl("范围过大（%d 条），最多 %d 条", "range too large (%d), max %d"), hi-lo+1, maxRange)
	}
	info, err := j.linkChat(a, map[string]*chatInfo{})
	if err != nil {
		return err
	}
	total := hi - lo + 1
	j.progress(fmt.Sprintf("🔄 "+j.tl("开始处理消息范围 %d-%d（共 %d 条）…", "Processing range %d-%d (%d messages)…"), lo, hi, total), true)
	for start := lo; start <= hi; start += 100 {
		end := start + 99
		if end > hi {
			end = hi
		}
		ids := make([]int, 0, end-start+1)
		for id := start; id <= end; id++ {
			ids = append(ids, id)
		}
		var msgs map[int]*tg.Message
		if err := j.call(func() (e error) { msgs, e = fetchMessages(j.jctx, j.api, info, ids); return }); err != nil {
			return err
		}
		j.skipped += len(ids) - len(msgs)
		var batch []*tg.Message
		for _, id := range sortedKeys(msgs) {
			batch = append(batch, msgs[id])
		}
		if len(batch) == 0 {
			continue
		}
		prefix := fmt.Sprintf("[%d/%d] ", end-lo+1, total)
		if j.local {
			for i, m := range batch {
				if j.jctx.Err() != nil {
					return j.jctx.Err()
				}
				j.progress(fmt.Sprintf("%s💾 "+j.tl("保存消息 %d…", "Saving message %d…"), fmt.Sprintf("[%d/%d] ", m.ID-lo+1, total), m.ID), i == 0)
				j.saveLocalOne(info, m, "")
			}
		} else {
			j.processUnit(unit{info: info, msgs: batch}, prefix)
		}
		if end < hi {
			if err := sleep(j.jctx, unitDelay); err != nil {
				return err
			}
		}
	}
	return j.jctx.Err()
}

// processUnit saves one message/album (or a range batch) to the target.
func (j *job) processUnit(u unit, prefix string) {
	if j.local {
		groupDir := ""
		if len(u.msgs) > 1 {
			if gid, ok := u.msgs[0].GetGroupedID(); ok && gid != 0 {
				groupDir = fmt.Sprintf("group_%d", gid)
			}
		}
		for i, m := range u.msgs {
			j.progress(fmt.Sprintf("%s💾 "+j.tl("正在保存媒体到本地 (%d/%d)…", "Saving media locally (%d/%d)…"), prefix, i+1, len(u.msgs)), i == 0)
			j.saveLocalOne(u.info, m, groupDir)
		}
		return
	}
	j.progress(prefix+"🔄 "+j.tl("尝试直接转发…", "Trying to forward…"), true)
	var upd tg.UpdatesClass
	err := j.call(func() (e error) {
		upd, e = j.api.MessagesForwardMessages(j.jctx, &tg.MessagesForwardMessagesRequest{
			FromPeer: u.info.Peer,
			ID:       u.ids(),
			RandomID: randomIDs(len(u.msgs)),
			ToPeer:   j.dest,
		})
		return
	})
	if err == nil {
		j.ok += len(u.msgs)
		if id := sentID(upd); id > 0 {
			j.lastSent = id
		}
		for _, m := range u.msgs {
			j.sources = append(j.sources, srcRef{u.info, m.ID})
		}
		return
	}
	if j.jctx.Err() != nil {
		return
	}
	if !isRestricted(err) {
		for _, m := range u.msgs {
			j.fail(fmt.Sprintf("#%d", m.ID), err)
		}
		return
	}
	// Forwarding is restricted: re-send content.
	for i, m := range u.msgs {
		if j.jctx.Err() != nil {
			return
		}
		sub := prefix
		if len(u.msgs) > 1 {
			sub += fmt.Sprintf("[%d/%d] ", i+1, len(u.msgs))
		}
		id, err := j.resend(m, sub)
		switch {
		case err == nil:
			j.ok++
			j.lastSent = id
			j.sources = append(j.sources, srcRef{u.info, m.ID})
		case errors.Is(err, errNoContent):
			j.skipped++
		default:
			j.fail(fmt.Sprintf("#%d", m.ID), err)
		}
	}
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// resend copies a message to the target without forwarding.
func (j *job) resend(m *tg.Message, prefix string) (int, error) {
	text := m.Message
	ents := sendableEntities(m.Entities)
	mi := downloadable(m)
	if mi == nil {
		if text == "" {
			return 0, errNoContent
		}
		j.progress(prefix+"📝 "+j.tl("转发受限，发送文本内容…", "Forwarding restricted, sending text…"), true)
		return j.sendText(text, ents, 0, m.Media == nil)
	}
	j.progress(fmt.Sprintf("%s⏬ "+j.tl("转发受限，下载%s…", "Forwarding restricted, downloading %s…"), prefix, mi.Kind), true)
	path, err := downloadTo(j.jctx, j.api, mi, j.p.tmpDir, m.ID)
	if err != nil {
		return 0, fmt.Errorf("download: %w", err)
	}
	defer os.Remove(path)
	j.progress(fmt.Sprintf("%s📤 "+j.tl("上传%s…", "Uploading %s…"), prefix, mi.Kind), true)
	media, err := reuploadMedia(j.jctx, j.api, mi, path)
	if err != nil {
		return 0, err
	}
	caption, capEnts := text, ents
	extraText := false
	if mi.Kind == "sticker" || utf16Len(text) > maxCaptionLen {
		caption, capEnts, extraText = "", nil, text != ""
	}
	req := &tg.MessagesSendMediaRequest{Peer: j.dest, Media: media, Message: caption, RandomID: randomID()}
	if len(capEnts) > 0 {
		req.SetEntities(capEnts)
	}
	var upd tg.UpdatesClass
	if err := j.call(func() (e error) { upd, e = j.api.MessagesSendMedia(j.jctx, req); return }); err != nil {
		plain, ok := plainDocument(media, mi)
		if !ok || j.jctx.Err() != nil {
			return 0, err
		}
		// Fallback: send as a plain file.
		req.Media, req.RandomID = plain, randomID()
		if err2 := j.call(func() (e error) { upd, e = j.api.MessagesSendMedia(j.jctx, req); return }); err2 != nil {
			// Keep both: why the media send was rejected and why the plain
			// fallback failed, so rejections are diagnosable.
			return 0, fmt.Errorf("%w; plain-file fallback: %v", err, err2)
		}
	}
	id := sentID(upd)
	if extraText {
		// The overflow text is sent as a reply to the media; lastSent must
		// stay the media id so the source card replies to the media, not
		// to this text follow-up.
		if _, err := j.sendText(text, ents, id, true); err != nil {
			j.fail(fmt.Sprintf("#%d", m.ID), err)
		}
	}
	return id, nil
}

func (j *job) sendText(text string, ents []tg.MessageEntityClass, replyTo int, noWebpage bool) (int, error) {
	req := &tg.MessagesSendMessageRequest{Peer: j.dest, Message: text, RandomID: randomID(), NoWebpage: noWebpage}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	if replyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	var upd tg.UpdatesClass
	if err := j.call(func() (e error) { upd, e = j.api.MessagesSendMessage(j.jctx, req); return }); err != nil {
		return 0, err
	}
	return sentID(upd), nil
}

// ---------------------------------------------------------------- source info

func linkOrID(info *chatInfo, id int) string {
	if l := info.link(id); l != "" {
		return plugin.Link(strconv.Itoa(id), l)
	}
	return plugin.Code(id)
}

func (j *job) sendSourceInfo(isRange bool) {
	var text string
	switch {
	case isRange:
		first, last := j.sources[0], j.sources[len(j.sources)-1]
		text = "🔗 **" + j.tl("范围保存来源", "Range Source") + "**\n\n" +
			"**▶️ " + j.tl("起始消息", "First message") + "**\n" + plugin.Bold(first.info.label()) + " / " + linkOrID(first.info, first.msgID) + "\n\n" +
			"**⏹ " + j.tl("结尾消息", "Last message") + "**\n" + plugin.Bold(last.info.label()) + " / " + linkOrID(last.info, last.msgID)
	case len(j.sources) == 1:
		s := j.sources[0]
		var b strings.Builder
		b.WriteString("📎 **" + j.tl("消息来源", "Message Source") + "**\n\n")
		if l := s.info.link(s.msgID); l != "" {
			b.WriteString("📝 " + plugin.Link(j.tl("查看原消息", "Open original message"), l) + "\n")
		}
		b.WriteString("👤 " + j.tl("来源对话  ", "Chat  ") + plugin.Bold(s.info.label()) + "\n")
		b.WriteString("#️⃣ " + j.tl("消息ID  ", "Message ID  ") + plugin.Code(s.msgID))
		text = b.String()
	default:
		type group struct {
			info *chatInfo
			ids  []int
		}
		groups := map[int64]*group{}
		var order []int64
		for _, s := range j.sources {
			g, ok := groups[s.info.ChatID]
			if !ok {
				g = &group{info: s.info}
				groups[s.info.ChatID] = g
				order = append(order, s.info.ChatID)
			}
			g.ids = append(g.ids, s.msgID)
		}
		sort.SliceStable(order, func(a, b int) bool { return groups[order[a]].info.label() < groups[order[b]].info.label() })
		var b strings.Builder
		b.WriteString("🔗 **" + j.tl("批量保存来源", "Batch Source") + "**\n")
		for _, cid := range order {
			g := groups[cid]
			uniq := map[int]bool{}
			for _, id := range g.ids {
				uniq[id] = true
			}
			var parts []string
			for _, r := range compactIDs(g.ids) {
				if r.Start == r.End {
					parts = append(parts, linkOrID(g.info, r.Start))
				} else {
					parts = append(parts, linkOrID(g.info, r.Start)+"-"+linkOrID(g.info, r.End))
				}
			}
			b.WriteString(fmt.Sprintf("\n👤 %s"+j.tl("（%d 条）：", " (%d): "), plugin.Bold(g.info.label()), len(uniq)))
			b.WriteString(strings.Join(parts, ", "))
		}
		text = b.String()
	}
	plain, ents := plugin.ParseMarkdown(text, nil)
	_, _ = j.sendText(plain, ents, j.lastSent, true)
}

// ---------------------------------------------------------------- local mode

type localFile struct {
	ChatID     int64
	ChatTitle  string
	MsgID      int
	Link       string
	Kind       string
	GroupedID  int64
	FilePath   string
	MetaPath   string
	Size       int64
	OrigName   string
	Caption    string
	SavedAtRFC string
}

func uniquePath(dir, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	cand := filepath.Join(dir, name)
	for i := 1; ; i++ {
		if _, err := os.Lstat(cand); errors.Is(err, os.ErrNotExist) {
			return cand
		}
		cand = filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, i, ext))
	}
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

func (j *job) chatDir(info *chatInfo) string {
	return filepath.Join(j.p.localDir, sanitizeSegment(strconv.FormatInt(info.ChatID, 10), "chat"))
}

func (j *job) saveLocalOne(info *chatInfo, m *tg.Message, groupDir string) {
	mi := downloadable(m)
	if mi == nil {
		j.skipped++
		return
	}
	lf, err := j.saveLocal(info, m, mi, groupDir)
	if err != nil {
		j.fail(fmt.Sprintf("#%d", m.ID), err)
		return
	}
	j.ok++
	j.files = append(j.files, lf)
}

func (j *job) saveLocal(info *chatInfo, m *tg.Message, mi *mediaInfo, groupDir string) (*localFile, error) {
	dir := j.chatDir(info)
	if groupDir != "" {
		dir = filepath.Join(dir, sanitizeSegment(groupDir, "group"))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := downloadTo(j.jctx, j.api, mi, j.p.tmpDir, m.ID)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	final := uniquePath(dir, fmt.Sprintf("msg_%d_%s%s", m.ID, mi.baseName(m.ID), mi.ext()))
	if err := moveFile(tmp, final); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	st, _ := os.Stat(final)
	lf := &localFile{
		ChatID: info.ChatID, ChatTitle: info.label(), MsgID: m.ID, Link: info.link(m.ID), Kind: mi.Kind,
		FilePath: final, OrigName: mi.FileName, Caption: m.Message, SavedAtRFC: time.Now().Format(time.RFC3339),
	}
	if st != nil {
		lf.Size = st.Size()
	}
	if gid, ok := m.GetGroupedID(); ok {
		lf.GroupedID = gid
	}
	meta := map[string]any{
		"savedAt": lf.SavedAtRFC,
		"source": map[string]any{
			"chatId": strconv.FormatInt(info.ChatID, 10), "chatTitle": lf.ChatTitle,
			"messageId": m.ID, "link": lf.Link, "date": time.Unix(int64(m.Date), 0).Format(time.RFC3339),
		},
		"media": map[string]any{
			"type": mi.Kind, "fileName": filepath.Base(final), "originalFileName": mi.FileName,
			"mimeType": mi.Mime, "fileSize": lf.Size, "caption": m.Message,
		},
	}
	metaPath := uniquePath(dir, strings.TrimSuffix(filepath.Base(final), filepath.Ext(final))+".json")
	if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
		if os.WriteFile(metaPath, b, 0o644) == nil {
			lf.MetaPath = metaPath
		}
	}
	return lf, nil
}

func (j *job) writeIndex() string {
	if len(j.files) == 0 {
		return ""
	}
	type fileEntry struct {
		SourceMessageID int     `json:"sourceMessageId"`
		SourceLink      string  `json:"sourceLink"`
		MediaType       string  `json:"mediaType"`
		GroupedID       *string `json:"groupedId"`
		FilePath        string  `json:"filePath"`
		MetadataPath    string  `json:"metadataPath"`
	}
	type chatEntry struct {
		ChatID    string      `json:"chatId"`
		ChatTitle string      `json:"chatTitle"`
		Count     int         `json:"count"`
		Files     []fileEntry `json:"files"`
	}
	byChat := map[int64]*chatEntry{}
	var order []int64
	for _, f := range j.files {
		c, ok := byChat[f.ChatID]
		if !ok {
			c = &chatEntry{ChatID: strconv.FormatInt(f.ChatID, 10), ChatTitle: f.ChatTitle}
			byChat[f.ChatID] = c
			order = append(order, f.ChatID)
		}
		e := fileEntry{SourceMessageID: f.MsgID, SourceLink: f.Link, MediaType: f.Kind, FilePath: f.FilePath, MetadataPath: f.MetaPath}
		if f.GroupedID != 0 {
			g := strconv.FormatInt(f.GroupedID, 10)
			e.GroupedID = &g
		}
		c.Files = append(c.Files, e)
		c.Count++
	}
	var chats []chatEntry
	for _, id := range order {
		c := byChat[id]
		sort.Slice(c.Files, func(a, b int) bool { return c.Files[a].SourceMessageID < c.Files[b].SourceMessageID })
		chats = append(chats, *c)
	}
	payload := map[string]any{"generatedAt": time.Now().Format(time.RFC3339), "totalFiles": len(j.files), "chats": chats}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(j.p.localDir, 0o755); err != nil {
		return ""
	}
	path := uniquePath(j.p.localDir, "index_"+strings.NewReplacer(":", "-", ".", "-").Replace(time.Now().Format("2006-01-02T15:04:05.000"))+".json")
	if os.WriteFile(path, b, 0o644) != nil {
		return ""
	}
	return path
}

// ---------------------------------------------------------------- summary

func (j *job) summary() string {
	tl := j.tl
	var b strings.Builder
	if j.local {
		index := j.writeIndex()
		if j.ok == 0 && j.failed == 0 {
			b.WriteString("⏭️ **" + tl("没有可保存的媒体", "No media to save") + "**\n\n")
			b.WriteString(tl("纯文本消息在 local 模式下跳过", "Text-only messages are skipped in local mode"))
			j.appendFailures(&b)
			return b.String()
		}
		b.WriteString("✅ **" + tl("本地保存完成", "Saved Locally") + "**\n\n")
		if len(j.files) == 1 && j.failed == 0 && j.skipped == 0 {
			f := j.files[0]
			b.WriteString(tl("文件", "File") + "  " + plugin.Code(f.FilePath) + "\n")
			if f.MetaPath != "" {
				b.WriteString(tl("元数据", "Metadata") + "  " + plugin.Code(f.MetaPath) + "\n")
			}
		} else {
			b.WriteString(tl("已保存", "Saved") + "  " + plugin.Code(j.ok) + "\n")
			b.WriteString(tl("跳过", "Skipped") + "  " + plugin.Code(j.skipped) + "\n")
			b.WriteString(tl("失败", "Failed") + "  " + plugin.Code(j.failed) + "\n")
		}
		dirs := map[string]bool{}
		for _, f := range j.files {
			dirs[filepath.Join(j.p.localDir, sanitizeSegment(strconv.FormatInt(f.ChatID, 10), "chat"))] = true
		}
		var ds []string
		for d := range dirs {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		for _, d := range ds {
			b.WriteString(tl("目录", "Folder") + "  " + plugin.Code(d) + "\n")
		}
		if index != "" {
			b.WriteString(tl("索引", "Index") + "  " + plugin.Code(index) + "\n")
		}
		j.appendFailures(&b)
		b.WriteString("\n💡 " + tl("每个媒体旁已生成同名 .json 来源元数据", "Each file has a .json sidecar with its source"))
		return b.String()
	}
	switch {
	case j.ok == 0 && j.failed == 0:
		b.WriteString("❌ " + tl("未找到可保存的消息", "Nothing to save"))
		return b.String()
	case j.ok == 0:
		b.WriteString("❌ **" + tl("保存失败", "Save Failed") + "**\n")
	case j.failed > 0:
		b.WriteString("⚠️ **" + tl("部分保存成功", "Partially Saved") + "**\n")
	default:
		b.WriteString("✅ **" + tl("保存完成", "Saved") + "**\n")
	}
	b.WriteString("\n" + tl("目标", "Target") + "  " + plugin.Code(j.label) + "\n")
	b.WriteString(tl("成功", "Saved") + "  " + plugin.Code(j.ok) + "\n")
	if j.skipped > 0 {
		b.WriteString(tl("跳过", "Skipped") + "  " + plugin.Code(j.skipped) + "\n")
	}
	if j.failed > 0 {
		b.WriteString(tl("失败", "Failed") + "  " + plugin.Code(j.failed) + "\n")
	}
	j.appendFailures(&b)
	return strings.TrimRight(b.String(), "\n")
}

func (j *job) appendFailures(b *strings.Builder) {
	if len(j.failures) == 0 {
		return
	}
	b.WriteString("\n**" + j.tl("错误", "Errors") + "**\n")
	for _, f := range j.failures {
		b.WriteString("❌ " + plugin.Escape(f) + "\n")
	}
	if strings.Contains(strings.Join(j.failures, " "), "CHANNEL_INVALID") || strings.Contains(strings.Join(j.failures, " "), "CHANNEL_PRIVATE") {
		b.WriteString("💡 " + j.tl("私有频道需先加入并在本账号中打开过", "Private channels must be joined and opened by this account first") + "\n")
	}
}
