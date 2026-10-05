// quote.go builds the quote-api payload from Telegram messages (payload
// construction + entity conversion), ported from the TeleBox yvlu source.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gotd/td/tg"
)

const quoteAPIURL = "https://quote-api-enhanced.zhetengsha.eu.org/generate.webp"

// ---------------------------------------------------------------- payload types

// quoteUser is the "from" object of a quoted message.
type quoteUser struct {
	ID          int64       `json:"id"`
	Name        string      `json:"name,omitempty"`
	FirstName   string      `json:"first_name,omitempty"`
	LastName    string      `json:"last_name,omitempty"`
	Username    string      `json:"username,omitempty"`
	Photo       *quotePhoto `json:"photo,omitempty"`
	EmojiStatus string      `json:"emoji_status,omitempty"`
}

type quotePhoto struct {
	URL string `json:"url"`
}

// quoteEntityUser is the nested user of a text_mention entity.
type quoteEntityUser struct {
	ID int64 `json:"id"`
}

// quoteEntity mirrors quote-api's entity format; offsets/lengths are UTF-16
// code units on both sides and pass through unchanged.
type quoteEntity struct {
	Offset        int              `json:"offset"`
	Length        int              `json:"length"`
	Type          string           `json:"type"`
	Language      string           `json:"language,omitempty"`
	CustomEmojiID string           `json:"custom_emoji_id,omitempty"`
	URL           string           `json:"url,omitempty"`
	User          *quoteEntityUser `json:"user,omitempty"`
}

// quoteMessage is one quoted message.
type quoteMessage struct {
	From          *quoteUser       `json:"from,omitempty"`
	Text          string           `json:"text"`
	Entities      []quoteEntity    `json:"entities,omitempty"`
	Avatar        bool             `json:"avatar"`
	ReplyMessage  *quoteReplyBlock `json:"replyMessage,omitempty"`
	Media         *quoteMedia      `json:"media,omitempty"`
	Voice         *quoteVoice      `json:"voice,omitempty"`
	Audio         *quoteAudio      `json:"audio,omitempty"`
	Document      *quoteDocument   `json:"document,omitempty"`
	MediaType     string           `json:"mediaType,omitempty"`
	MediaDuration int              `json:"mediaDuration,omitempty"`
	Forward       *quoteForward    `json:"forward,omitempty"`
}

type quoteReplyBlock struct {
	Name     string        `json:"name"`
	Text     string        `json:"text"`
	Entities []quoteEntity `json:"entities,omitempty"`
	ChatID   int64         `json:"chatId,omitempty"`
}

type quoteMedia struct {
	URL string `json:"url"` // data: URI
}

type quoteVoice struct {
	Waveform []int `json:"waveform"`
	Duration int   `json:"duration,omitempty"`
}

type quoteAudio struct {
	Title     string `json:"title"`
	Performer string `json:"performer,omitempty"`
	Duration  int    `json:"duration,omitempty"`
}

type quoteDocument struct {
	FileName string `json:"file_name"`
}

type quoteForward struct {
	Label string `json:"label"`
}

// quoteRequest is the full generate request.
type quoteRequest struct {
	Type            string         `json:"type"`
	Format          string         `json:"format"`
	BackgroundColor string         `json:"backgroundColor"`
	Width           int            `json:"width"`
	Height          int            `json:"height"`
	Scale           int            `json:"scale"`
	EmojiBrand      string         `json:"emojiBrand"`
	Messages        []quoteMessage `json:"messages"`
}

// buildRequest assembles the request for the chosen output format
// ("" and "webp" are the same sticker quote).
func buildRequest(format string, items []quoteMessage) *quoteRequest {
	req := &quoteRequest{
		Type: "quote", Format: "webp",
		BackgroundColor: "#1b1429",
		Width:           512, Height: 768, Scale: 2,
		EmojiBrand: "apple",
		Messages:   items,
	}
	switch format {
	case "stories":
		req.Type, req.Format, req.Width, req.Height = "stories", "png", 360, 640
	case "image":
		req.Type, req.Format = "image", "png"
	}
	return req
}

// ---------------------------------------------------------------- entity mapping

// convertEntities maps tg entities to quote-api entities.
func convertEntities(ents []tg.MessageEntityClass) []quoteEntity {
	out := make([]quoteEntity, 0, len(ents))
	for _, e := range ents {
		var q quoteEntity
		switch v := e.(type) {
		case *tg.MessageEntityBold:
			q = quoteEntity{Type: "bold"}
		case *tg.MessageEntityItalic:
			q = quoteEntity{Type: "italic"}
		case *tg.MessageEntityUnderline:
			q = quoteEntity{Type: "underline"}
		case *tg.MessageEntityStrike:
			q = quoteEntity{Type: "strikethrough"}
		case *tg.MessageEntityCode:
			q = quoteEntity{Type: "code"}
		case *tg.MessageEntityPre:
			q = quoteEntity{Type: "pre", Language: v.Language}
		case *tg.MessageEntityCustomEmoji:
			q = quoteEntity{Type: "custom_emoji", CustomEmojiID: fmt.Sprintf("%d", v.DocumentID)}
		case *tg.MessageEntityURL:
			q = quoteEntity{Type: "url"}
		case *tg.MessageEntityTextURL:
			q = quoteEntity{Type: "text_link", URL: v.URL}
		case *tg.MessageEntityMention:
			q = quoteEntity{Type: "mention"}
		case *tg.MessageEntityMentionName:
			q = quoteEntity{Type: "text_mention", User: &quoteEntityUser{ID: v.UserID}}
		case *tg.MessageEntityHashtag:
			q = quoteEntity{Type: "hashtag"}
		case *tg.MessageEntityCashtag:
			q = quoteEntity{Type: "cashtag"}
		case *tg.MessageEntityBotCommand:
			q = quoteEntity{Type: "bot_command"}
		case *tg.MessageEntityEmail:
			q = quoteEntity{Type: "email"}
		case *tg.MessageEntityPhone:
			q = quoteEntity{Type: "phone_number"}
		case *tg.MessageEntitySpoiler:
			q = quoteEntity{Type: "spoiler"}
		case *tg.MessageEntityBlockquote:
			q = quoteEntity{Type: "blockquote"}
		default:
			continue
		}
		q.Offset, q.Length = e.GetOffset(), e.GetLength()
		if q.Length > 0 {
			out = append(out, q)
		}
	}
	return out
}

// ---------------------------------------------------------------- message env

// msgEnv caches users/chats for one quote run and fetches messages, avatars
// and media. It lives for a single command invocation.
type msgEnv struct {
	ctx   context.Context
	api   *tg.Client
	peer  tg.InputPeerClass
	users map[int64]*tg.User
	chats map[int64]tg.ChatClass
	tmp   string
}

func newEnv(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, tmp string) *msgEnv {
	return &msgEnv{ctx: ctx, api: api, peer: peer, users: map[int64]*tg.User{}, chats: map[int64]tg.ChatClass{}, tmp: tmp}
}

// absorb merges users/chats returned by an API response.
func (e *msgEnv) absorb(users []tg.UserClass, chats []tg.ChatClass) {
	for _, u := range users {
		if v, ok := u.(*tg.User); ok {
			e.users[v.ID] = v
		}
	}
	for _, c := range chats {
		switch v := c.(type) {
		case *tg.Chat:
			e.chats[v.ID] = v
		case *tg.Channel:
			e.chats[v.ID] = v
		case *tg.ChannelForbidden:
			e.chats[v.ID] = v
		}
	}
}

// fetchOne gets a single message by id (channel-aware), absorbing entities.
func (e *msgEnv) fetchOne(id int) (*tg.Message, error) {
	if e.api == nil {
		return nil, errors.New("no api client")
	}
	var (
		res tg.MessagesMessagesClass
		err error
	)
	in := []tg.InputMessageClass{&tg.InputMessageID{ID: id}}
	if ch, ok := e.peer.(*tg.InputPeerChannel); ok {
		res, err = e.api.ChannelsGetMessages(e.ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
			ID:      in,
		})
	} else {
		res, err = e.api.MessagesGetMessages(e.ctx, in)
	}
	if err != nil {
		return nil, err
	}
	mod, ok := res.AsModified()
	if !ok {
		return nil, fmt.Errorf("message %d not found", id)
	}
	e.absorb(mod.GetUsers(), mod.GetChats())
	for _, m := range mod.GetMessages() {
		if msg, ok := m.(*tg.Message); ok && msg.ID == id {
			return msg, nil
		}
	}
	return nil, fmt.Errorf("message %d not found", id)
}

// fetchFollowing returns the messages right after baseID, oldest first
// (messages.getHistory with a negative add_offset, matching the source's
// reverse history fetch). count includes baseID itself.
func (e *msgEnv) fetchFollowing(baseID, count int) ([]*tg.Message, error) {
	if e.api == nil {
		return nil, errors.New("no api client")
	}
	n := count - 1
	if n <= 0 {
		return nil, nil
	}
	res, err := e.api.MessagesGetHistory(e.ctx, &tg.MessagesGetHistoryRequest{
		Peer:      e.peer,
		OffsetID:  baseID,
		AddOffset: -n,
		Limit:     n,
		MinID:     baseID,
	})
	if err != nil {
		return nil, err
	}
	mod, ok := res.AsModified()
	if !ok {
		return nil, nil
	}
	e.absorb(mod.GetUsers(), mod.GetChats())
	var out []*tg.Message
	for _, m := range mod.GetMessages() {
		if msg, ok := m.(*tg.Message); ok && msg.ID > baseID {
			out = append(out, msg)
		}
	}
	// oldest first
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].ID > out[j].ID; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out, nil
}

// senderOf resolves the user (or channel) a message came from, refreshing min
// users so emoji_status is available (the source's ensureFullUser).
func (e *msgEnv) senderOf(msg *tg.Message) (*tg.User, *tg.Channel, error) {
	if uid, ok := msg.FromID.(*tg.PeerUser); ok {
		if u, ok := e.users[uid.UserID]; ok && !u.Min {
			return u, nil, nil
		}
		if e.api != nil {
			users, err := e.api.UsersGetUsers(e.ctx, []tg.InputUserClass{&tg.InputUser{UserID: uid.UserID}})
			if err == nil && len(users) > 0 {
				if u, ok := users[0].(*tg.User); ok {
					e.users[u.ID] = u
					return u, nil, nil
				}
			}
		}
		if u, ok := e.users[uid.UserID]; ok {
			return u, nil, nil
		}
		return nil, nil, fmt.Errorf("user %d not found", uid.UserID)
	}
	if cid, ok := msg.FromID.(*tg.PeerChannel); ok {
		if c, ok := e.chats[cid.ChannelID]; ok {
			if ch, ok := c.(*tg.Channel); ok {
				return nil, ch, nil
			}
		}
		return nil, nil, fmt.Errorf("channel %d not found", cid.ChannelID)
	}
	// no FromID: private chat peer or anonymous admin
	if uid, ok := msg.PeerID.(*tg.PeerUser); ok {
		if u, ok := e.users[uid.UserID]; ok {
			return u, nil, nil
		}
	}
	return nil, nil, errors.New("cannot resolve the message sender")
}

// userName composes the display name of a user.
func userName(u *tg.User) string {
	n := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if n == "" {
		n = u.Username
	}
	return n
}

// avatar downloads the user's current profile photo (limit 1, like the
// source's getProfilePhotos).
func (e *msgEnv) avatar(u *tg.User) ([]byte, error) {
	if e.api == nil {
		return nil, errors.New("no api client")
	}
	in := u.AsInput()
	if in.UserID == 0 {
		return nil, errors.New("no access hash")
	}
	res, err := e.api.PhotosGetUserPhotos(e.ctx, &tg.PhotosGetUserPhotosRequest{
		UserID: in, Limit: 1,
	})
	if err != nil {
		return nil, err
	}
	var photos []tg.PhotoClass
	switch v := res.(type) {
	case *tg.PhotosPhotos:
		photos = v.Photos
	case *tg.PhotosPhotosSlice:
		photos = v.Photos
	}
	if len(photos) == 0 {
		return nil, errors.New("no photo")
	}
	photo, ok := photos[0].(*tg.Photo)
	if !ok {
		return nil, errors.New("no photo")
	}
	typ, _, _, _ := largestPhotoSize(photo.Sizes)
	if typ == "" {
		return nil, errors.New("no photo size")
	}
	loc := &tg.InputPhotoFileLocation{
		ID: photo.ID, AccessHash: photo.AccessHash,
		FileReference: photo.FileReference, ThumbSize: typ,
	}
	return downloadBytes(e.ctx, e.api, loc)
}

// replyBlock builds the replyMessage context (yvlu r): an explicit quote
// selection first, then the actually replied message (source behaviour).
func (e *msgEnv) replyBlock(msg *tg.Message) (*quoteReplyBlock, bool) {
	hdr, ok := msg.ReplyTo.(*tg.MessageReplyHeader)
	if !ok || hdr.ReplyToMsgID == 0 {
		return nil, false
	}
	if hdr.Quote && strings.TrimSpace(hdr.QuoteText) != "" {
		rb := &quoteReplyBlock{Name: "unknown", Text: hdr.QuoteText, Entities: convertEntities(hdr.QuoteEntities)}
		if replied, err := e.fetchOne(hdr.ReplyToMsgID); err == nil {
			if u, _, err := e.senderOf(replied); err == nil && u != nil {
				rb.Name = userName(u)
				rb.ChatID = u.ID
			}
		}
		return rb, true
	}
	replied, err := e.fetchOne(hdr.ReplyToMsgID)
	if err != nil {
		return nil, false
	}
	text := strings.TrimSpace(replied.Message)
	if text == "" {
		return nil, false
	}
	rb := &quoteReplyBlock{Name: "unknown", Text: text, Entities: convertEntities(replied.Entities)}
	if u, _, err := e.senderOf(replied); err == nil && u != nil {
		rb.Name = userName(u)
		rb.ChatID = u.ID
	}
	return rb, true
}

// ---------------------------------------------------------------- build items

// quotedFragment overrides the first message's text when the command was
// sent as a reply with a text selection.
type quotedFragment struct {
	text     string
	entities []tg.MessageEntityClass
}

// buildItems turns the messages into quote-api message objects, hiding the
// avatar (and name) when the sender repeats, like the source.
func buildItems(e *msgEnv, msgs []*tg.Message, opts yvluArgs, frag *quotedFragment) []quoteMessage {
	items := make([]quoteMessage, 0, len(msgs))
	prevSender := ""
	for i, msg := range msgs {
		items = append(items, buildItem(e, msg, opts, frag, i, &prevSender))
	}
	return items
}

// buildItemsChecked wraps buildItems: it fails when no message yielded a
// usable sender (mirrors the source's "无法获取消息发送者信息").
func buildItemsChecked(e *msgEnv, msgs []*tg.Message, opts yvluArgs, frag *quotedFragment) ([]quoteMessage, error) {
	items := buildItems(e, msgs, opts, frag)
	if len(items) == 0 {
		return nil, errors.New("no messages to quote")
	}
	return items, nil
}

func buildItem(e *msgEnv, msg *tg.Message, opts yvluArgs, frag *quotedFragment, idx int, prevSender *string) quoteMessage {
	from := &quoteUser{}
	user, channel, serr := e.senderOf(msg)
	if serr != nil {
		// forwarded message with no local sender: fall back to the header
		if fwd, ok := forwardHeader(msg); ok {
			name := firstNonEmpty(fwdLabel(fwd), "Forwarded")
			from.ID = int64(hashString(name))
			from.Name = name
		} else {
			from.ID = int64(hashString(fmt.Sprintf("user_%d", msg.ID)))
		}
	} else if user != nil {
		from.ID = user.ID
		from.Username = user.Username
		if st, ok := user.GetEmojiStatus(); ok {
			if es, ok := st.(*tg.EmojiStatus); ok && es.DocumentID != 0 {
				from.EmojiStatus = fmt.Sprintf("%d", es.DocumentID)
			}
		}
	} else if channel != nil {
		from.ID = -channel.ID
	}

	ident := fmt.Sprintf("%d", from.ID)
	showAvatar := ident != *prevSender
	*prevSender = ident
	if showAvatar {
		if user != nil {
			from.Name = userName(user)
			from.FirstName, from.LastName = user.FirstName, user.LastName
			// only user senders get a photo, fetched just for new senders
			if data, err := e.avatar(user); err == nil && len(data) > 0 {
				from.Photo = &quotePhoto{URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)}
			}
		} else if channel != nil {
			from.Name = channel.Title
		}
	}
	return assembleItem(from, msg, opts, frag, idx, showAvatar, e)
}

func assembleItem(from *quoteUser, msg *tg.Message, opts yvluArgs, frag *quotedFragment, idx int, avatar bool, e *msgEnv) quoteMessage {
	text, entities := msg.Message, msg.Entities
	if idx == 0 && frag != nil {
		text, entities = frag.text, frag.entities
	}
	item := quoteMessage{
		From:     from,
		Text:     text,
		Entities: convertEntities(entities),
		Avatar:   avatar,
	}
	if opts.withReply {
		if rb, ok := e.replyBlock(msg); ok {
			item.ReplyMessage = rb
		}
	}
	if uri, ok := mediaDataURI(e, msg); ok {
		item.Media = &quoteMedia{URL: uri}
	}
	fillAdvanced(&item, msg)
	if fwd, ok := forwardHeader(msg); ok {
		if label := fwdLabel(fwd); label != "" {
			item.Forward = &quoteForward{Label: label}
		}
	}
	return item
}

// forwardHeader returns the fwd header when the message is forwarded.
func forwardHeader(msg *tg.Message) (*tg.MessageFwdHeader, bool) {
	if msg.FwdFrom.Zero() {
		return nil, false
	}
	return &msg.FwdFrom, true
}

// fwdLabel picks the best display name from a forward header.
func fwdLabel(fwd *tg.MessageFwdHeader) string {
	return firstNonEmpty(fwd.FromName, fwd.PostAuthor)
}

// hashString is a small stable string hash (Java-style), like the source's
// hashCode used for anonymous senders.
func hashString(s string) int32 {
	var h int32
	for _, c := range []byte(s) {
		h = h<<5 - h + int32(c)
	}
	return h
}

// fillAdvanced sets voice/audio/document/mediaType fields from the message
// media (quote-api "glass" fields).
func fillAdvanced(item *quoteMessage, msg *tg.Message) {
	md, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return
	}
	d, ok := md.Document.(*tg.Document)
	if !ok {
		return
	}
	var (
		audio    *tg.DocumentAttributeAudio
		video    *tg.DocumentAttributeVideo
		animated bool
		fileName string
	)
	for _, a := range d.Attributes {
		switch v := a.(type) {
		case *tg.DocumentAttributeAudio:
			audio = v
		case *tg.DocumentAttributeVideo:
			video = v
		case *tg.DocumentAttributeAnimated:
			animated = true
		case *tg.DocumentAttributeFilename:
			fileName = v.FileName
		}
	}
	switch {
	case audio != nil && audio.Voice:
		// waveform bytes are bitpacked 5-bit values (v << 3); quote-api wants 0..31
		wf := make([]int, 0, len(audio.Waveform))
		for _, b := range audio.Waveform {
			wf = append(wf, min(int(b)>>3, 31))
		}
		item.Voice = &quoteVoice{Waveform: wf, Duration: audio.Duration}
	case audio != nil:
		item.Audio = &quoteAudio{
			Title:     firstNonEmpty(audio.Title, fileName, "Audio"),
			Performer: audio.Performer,
			Duration:  audio.Duration,
		}
	case video != nil:
		// animation -> gif, video and round video -> video (source mapping)
		item.MediaType = map[bool]string{true: "gif", false: "video"}[animated]
		item.MediaDuration = int(video.Duration)
	default:
		if fileName != "" {
			item.Document = &quoteDocument{FileName: fileName}
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------- api call

// imageKind identifies quote-api output bytes by magic numbers.
func imageKind(b []byte) (string, error) {
	switch {
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp", nil
	case len(b) >= 8 && string(b[0:8]) == "\x89PNG\r\n\x1a\n":
		return "png", nil
	case len(b) >= 4 && b[0] == 0x1a && b[1] == 0x45 && b[2] == 0xdf && b[3] == 0xa3:
		return "webm", nil
	}
	preview := strings.Join(strings.Fields(string(truncBytes(b, 120))), " ")
	if preview != "" {
		return "", fmt.Errorf("quote-api returned non-image data: %s", truncRunes(preview, 100))
	}
	return "", errors.New("quote-api returned non-image data")
}

func truncBytes(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// postQuote sends the generate request and returns the raw image bytes plus
// the detected kind ("webp", "png" or "webm").
func (p *YvluPlugin) postQuote(ctx context.Context, req *quoteRequest) ([]byte, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, "", err
	}
	c, cancel := context.WithTimeout(ctx, quoteTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(c, http.MethodPost, quoteAPIURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("User-Agent", "PaperValet/yvlu")
	resp, err := p.http.Do(hreq)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := strings.Join(strings.Fields(string(truncBytes(data, 160))), " ")
		if detail != "" {
			return nil, "", fmt.Errorf("quote-api HTTP %d: %s", resp.StatusCode, truncRunes(detail, 120))
		}
		return nil, "", fmt.Errorf("quote-api HTTP %d", resp.StatusCode)
	}
	if len(data) == 0 {
		return nil, "", errors.New("quote-api returned an empty body")
	}
	kind, err := imageKind(data)
	if err != nil {
		return nil, "", err
	}
	return data, kind, nil
}
