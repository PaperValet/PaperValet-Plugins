package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

// chatInfo describes a source chat.
type chatInfo struct {
	Peer     tg.InputPeerClass
	ChatID   int64 // PaperValet chat id form
	Username string
	Title    string
}

func (c *chatInfo) label() string {
	if c.Title != "" {
		return c.Title
	}
	if c.Username != "" {
		return "@" + c.Username
	}
	return fmt.Sprint(c.ChatID)
}

func (c *chatInfo) link(msgID int) string { return messageLink(c.ChatID, c.Username, msgID) }

// chatIDOf converts an InputPeer to the PaperValet chat id form.
func chatIDOf(p tg.InputPeerClass) int64 {
	switch v := p.(type) {
	case *tg.InputPeerChannel:
		return channelChatID(v.ChannelID)
	case *tg.InputPeerChat:
		return -v.ChatID
	case *tg.InputPeerUser:
		return v.UserID
	}
	return 0
}

// chatIDOfPeer converts a Peer to the PaperValet chat id form.
func chatIDOfPeer(p tg.PeerClass) int64 {
	switch v := p.(type) {
	case *tg.PeerChannel:
		return channelChatID(v.ChannelID)
	case *tg.PeerChat:
		return -v.ChatID
	case *tg.PeerUser:
		return v.UserID
	}
	return 0
}

// fillInfo sets the chat title/username from entities returned by the API.
func fillInfo(info *chatInfo, chats []tg.ChatClass, users []tg.UserClass) {
	for _, c := range chats {
		switch v := c.(type) {
		case *tg.Channel:
			if channelChatID(v.ID) == info.ChatID {
				info.Title = v.Title
				if info.Username == "" {
					info.Username = v.Username
				}
				if pc, ok := info.Peer.(*tg.InputPeerChannel); ok && pc.AccessHash == 0 && v.AccessHash != 0 {
					info.Peer = &tg.InputPeerChannel{ChannelID: v.ID, AccessHash: v.AccessHash}
				}
			}
		case *tg.Chat:
			if -v.ID == info.ChatID {
				info.Title = v.Title
			}
		case *tg.ChannelForbidden:
			if channelChatID(v.ID) == info.ChatID {
				info.Title = v.Title
			}
		}
	}
	for _, u := range users {
		if v, ok := u.(*tg.User); ok && v.ID == info.ChatID {
			info.Title = strings.TrimSpace(v.FirstName + " " + v.LastName)
			if info.Title == "" {
				info.Title = v.Username
			}
		}
	}
}

// fetchMessages fetches up to 100 messages by id. Missing/empty messages
// are absent from the result map.
func fetchMessages(ctx context.Context, api *tg.Client, info *chatInfo, ids []int) (map[int]*tg.Message, error) {
	out := map[int]*tg.Message{}
	if len(ids) == 0 {
		return out, nil
	}
	var (
		res tg.MessagesMessagesClass
		err error
	)
	if ch, ok := info.Peer.(*tg.InputPeerChannel); ok {
		in := make([]tg.InputMessageClass, len(ids))
		for i, id := range ids {
			in[i] = &tg.InputMessageID{ID: id}
		}
		res, err = api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
			ID:      in,
		})
	} else {
		in := make([]tg.InputMessageClass, len(ids))
		for i, id := range ids {
			in[i] = &tg.InputMessageID{ID: id}
		}
		res, err = api.MessagesGetMessages(ctx, in)
	}
	if err != nil {
		return nil, err
	}
	mod, ok := res.AsModified()
	if !ok {
		return out, nil
	}
	fillInfo(info, mod.GetChats(), mod.GetUsers())
	for _, m := range mod.GetMessages() {
		msg, ok := m.(*tg.Message) // skips MessageEmpty and MessageService
		if !ok {
			continue
		}
		// messages.getMessages is global for users/basic groups; make sure
		// the message belongs to the requested chat.
		if _, self := info.Peer.(*tg.InputPeerSelf); !self && info.ChatID != 0 && chatIDOfPeer(msg.PeerID) != info.ChatID {
			continue
		}
		out[msg.ID] = msg
	}
	return out, nil
}

// fetchAlbum returns all messages of msg's media group, ordered by id.
func fetchAlbum(ctx context.Context, api *tg.Client, info *chatInfo, msg *tg.Message) ([]*tg.Message, error) {
	gid, ok := msg.GetGroupedID()
	if !ok || gid == 0 {
		return []*tg.Message{msg}, nil
	}
	var ids []int
	for id := msg.ID - 10; id <= msg.ID+10; id++ {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	got, err := fetchMessages(ctx, api, info, ids)
	if err != nil {
		return nil, err
	}
	var out []*tg.Message
	for _, id := range ids {
		if m, ok := got[id]; ok {
			if g, ok := m.GetGroupedID(); ok && g == gid {
				out = append(out, m)
			}
		}
	}
	if len(out) == 0 {
		out = []*tg.Message{msg}
	}
	return out, nil
}

// isRestricted reports whether a forward failed because the source chat
// forbids forwarding/saving (reference heuristic).
func isRestricted(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "CHAT_FORWARDS_RESTRICTED") || strings.Contains(s, "SAVE") ||
		strings.Contains(s, "FORWARD")
}

// sentID extracts the id of the last message created by an Updates result.
func sentID(u tg.UpdatesClass) int {
	id := 0
	var list []tg.UpdateClass
	switch v := u.(type) {
	case *tg.UpdateShortSentMessage:
		return v.ID
	case *tg.Updates:
		list = v.Updates
	case *tg.UpdatesCombined:
		list = v.Updates
	}
	for _, up := range list {
		switch x := up.(type) {
		case *tg.UpdateNewMessage:
			if x.Message.GetID() > id {
				id = x.Message.GetID()
			}
		case *tg.UpdateNewChannelMessage:
			if x.Message.GetID() > id {
				id = x.Message.GetID()
			}
		case *tg.UpdateMessageID:
			if x.ID > id {
				id = x.ID
			}
		}
	}
	return id
}

// sendableEntities drops mention-by-id entities, which need input users.
func sendableEntities(ents []tg.MessageEntityClass) []tg.MessageEntityClass {
	var out []tg.MessageEntityClass
	for _, e := range ents {
		if _, ok := e.(*tg.MessageEntityMentionName); ok {
			continue
		}
		out = append(out, e)
	}
	return out
}

// mediaInfo describes downloadable media of a message.
type mediaInfo struct {
	Kind     string // photo, video, gif, audio, voice, sticker, document
	Location tg.InputFileLocationClass
	FileName string
	Mime     string
	Size     int64
	Doc      *tg.Document
	Spoiler  bool
}

// downloadable returns media info, or nil when the message has no file
// (text, web page previews, polls, geo…).
func downloadable(msg *tg.Message) *mediaInfo {
	switch m := msg.Media.(type) {
	case *tg.MessageMediaPhoto:
		p, ok := m.Photo.(*tg.Photo)
		if !ok {
			return nil
		}
		typ, size := largestPhoto(p.Sizes)
		if typ == "" {
			return nil
		}
		return &mediaInfo{Kind: "photo", Mime: "image/jpeg", Size: size, Spoiler: m.Spoiler,
			Location: &tg.InputPhotoFileLocation{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference, ThumbSize: typ}}
	case *tg.MessageMediaDocument:
		d, ok := m.Document.(*tg.Document)
		if !ok {
			return nil
		}
		mi := &mediaInfo{Kind: "document", Mime: d.MimeType, Size: d.Size, Doc: d, Spoiler: m.Spoiler,
			Location: &tg.InputDocumentFileLocation{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}}
		animated := false
		for _, a := range d.Attributes {
			switch at := a.(type) {
			case *tg.DocumentAttributeFilename:
				mi.FileName = at.FileName
			case *tg.DocumentAttributeAnimated:
				animated = true
			case *tg.DocumentAttributeSticker:
				mi.Kind = "sticker"
			case *tg.DocumentAttributeVideo:
				if mi.Kind == "document" {
					mi.Kind = "video"
				}
			case *tg.DocumentAttributeAudio:
				if at.Voice {
					mi.Kind = "voice"
				} else if mi.Kind == "document" {
					mi.Kind = "audio"
				}
			}
		}
		if animated && mi.Kind == "video" {
			mi.Kind = "gif"
		}
		if mi.Kind == "document" {
			switch {
			case strings.HasPrefix(d.MimeType, "video/"):
				mi.Kind = "video"
			case strings.HasPrefix(d.MimeType, "audio/"):
				mi.Kind = "audio"
			case strings.HasPrefix(d.MimeType, "image/"):
				mi.Kind = "photo"
			}
		}
		return mi
	}
	return nil
}

func largestPhoto(sizes []tg.PhotoSizeClass) (string, int64) {
	bestType, best := "", int64(-1)
	for _, s := range sizes {
		switch v := s.(type) {
		case *tg.PhotoSize:
			if int64(v.Size) > best {
				bestType, best = v.Type, int64(v.Size)
			}
		case *tg.PhotoSizeProgressive:
			if n := len(v.Sizes); n > 0 && int64(v.Sizes[n-1]) > best {
				bestType, best = v.Type, int64(v.Sizes[n-1])
			}
		}
	}
	if best < 0 {
		return "", 0
	}
	return bestType, best
}

// baseName returns a safe file base name for media (without extension).
func (mi *mediaInfo) baseName(msgID int) string {
	if mi.FileName != "" {
		n := strings.TrimSuffix(filepath.Base(mi.FileName), filepath.Ext(mi.FileName))
		return sanitizeSegment(n, mi.Kind)
	}
	return fmt.Sprintf("%s_%d", mi.Kind, msgID)
}

func (mi *mediaInfo) ext() string { return extFor(mi.FileName, mi.Mime, mi.Kind) }

// downloadTo downloads media to a fresh temp file in dir.
func downloadTo(ctx context.Context, api *tg.Client, mi *mediaInfo, dir string, msgID int) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, fmt.Sprintf("dl-%d-*%s", msgID, mi.ext()))
	if err != nil {
		return "", err
	}
	path := f.Name()
	_, err = downloader.NewDownloader().Download(api, mi.Location).Stream(ctx, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if st, serr := os.Stat(path); serr != nil || st.Size() == 0 {
			err = errors.New("empty download")
		}
	}
	if err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// reuploadMedia builds InputMedia from a downloaded file, keeping the
// original document attributes (video/audio/sticker/file name).
func reuploadMedia(ctx context.Context, api *tg.Client, mi *mediaInfo, path string) (tg.InputMediaClass, error) {
	name := mi.baseName(0) + mi.ext()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	file, err := uploader.NewUploader(api).FromReader(ctx, name, f)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	if mi.Doc == nil {
		m := &tg.InputMediaUploadedPhoto{File: file}
		if mi.Spoiler {
			m.SetSpoiler(true)
		}
		return m, nil
	}
	mt := mi.Mime
	if mt == "" {
		mt = mime.TypeByExtension(mi.ext())
	}
	if mt == "" {
		mt = "application/octet-stream"
	}
	attrs := append([]tg.DocumentAttributeClass(nil), mi.Doc.Attributes...)
	hasName := false
	for _, a := range attrs {
		if _, ok := a.(*tg.DocumentAttributeFilename); ok {
			hasName = true
		}
	}
	if !hasName && mi.Kind == "document" {
		attrs = append(attrs, &tg.DocumentAttributeFilename{FileName: name})
	}
	m := &tg.InputMediaUploadedDocument{File: file, MimeType: mt, Attributes: attrs}
	if mi.Spoiler {
		m.SetSpoiler(true)
	}
	return m, nil
}

// plainDocument re-wraps an uploaded document as a generic file, used when
// Telegram rejects the original attributes (e.g. sticker metadata).
func plainDocument(m tg.InputMediaClass, mi *mediaInfo) (tg.InputMediaClass, bool) {
	d, ok := m.(*tg.InputMediaUploadedDocument)
	if !ok {
		return nil, false
	}
	out := &tg.InputMediaUploadedDocument{
		File:       d.File,
		MimeType:   d.MimeType,
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: mi.baseName(0) + mi.ext()}},
	}
	out.SetForceFile(true)
	return out, true
}

func randomID() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return int64(binary.LittleEndian.Uint64(b[:]))
	}
	return time.Now().UnixNano()
}

func randomIDs(n int) []int64 {
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = randomID()
	}
	return ids
}
