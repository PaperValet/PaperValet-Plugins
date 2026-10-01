package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

const (
	dataDir    = "data/sendat"
	tasksFile  = "tasks.json"
	configFile = "config.json"
)

type storeFile struct {
	NextID int     `json:"next_id"`
	Tasks  []*Task `json:"tasks"`
}

type config struct {
	Timezone string `json:"timezone,omitempty"`
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// writeJSON writes atomically (temp file + rename).
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// peerRef is a persisted InputPeer so restored tasks need no resolver.
type peerRef struct {
	Type       string `json:"type"` // user|chat|channel|self
	ID         int64  `json:"id"`
	AccessHash int64  `json:"access_hash,omitempty"`
}

func refFromPeer(p tg.InputPeerClass) *peerRef {
	switch v := p.(type) {
	case *tg.InputPeerUser:
		return &peerRef{Type: "user", ID: v.UserID, AccessHash: v.AccessHash}
	case *tg.InputPeerChat:
		return &peerRef{Type: "chat", ID: v.ChatID}
	case *tg.InputPeerChannel:
		return &peerRef{Type: "channel", ID: v.ChannelID, AccessHash: v.AccessHash}
	case *tg.InputPeerSelf:
		return &peerRef{Type: "self"}
	}
	return nil
}

func (r *peerRef) input() tg.InputPeerClass {
	if r == nil {
		return nil
	}
	switch r.Type {
	case "user":
		return &tg.InputPeerUser{UserID: r.ID, AccessHash: r.AccessHash}
	case "chat":
		return &tg.InputPeerChat{ChatID: r.ID}
	case "channel":
		return &tg.InputPeerChannel{ChannelID: r.ID, AccessHash: r.AccessHash}
	case "self":
		return &tg.InputPeerSelf{}
	}
	return nil
}

// utf16Len counts UTF-16 code units (Telegram entity offsets).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// sliceEntities keeps the entities covering text[base:] (UTF-16 offsets)
// and rebases them to start at 0. Entities partially before base are clipped.
func sliceEntities(ents []tg.MessageEntityClass, base, total int) []tg.MessageEntityClass {
	var out []tg.MessageEntityClass
	for _, e := range ents {
		start, end := e.GetOffset(), e.GetOffset()+e.GetLength()
		if end <= base || start >= total {
			continue
		}
		if start < base {
			start = base
		}
		if end > total {
			end = total
		}
		c := cloneEntity(e)
		if c == nil || !setOffsetLength(c, start-base, end-start) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func cloneEntity(e tg.MessageEntityClass) tg.MessageEntityClass {
	raw, err := encodeEntity(e)
	if err != nil {
		return nil
	}
	c, err := decodeEntity(raw)
	if err != nil {
		return nil
	}
	return c
}

// setOffsetLength updates Offset/Length on any concrete entity type.
func setOffsetLength(e tg.MessageEntityClass, off, length int) bool {
	v := reflect.ValueOf(e)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return false
	}
	v = v.Elem()
	o, l := v.FieldByName("Offset"), v.FieldByName("Length")
	if !o.IsValid() || !l.IsValid() || !o.CanSet() || !l.CanSet() || o.Kind() != reflect.Int || l.Kind() != reflect.Int {
		return false
	}
	o.SetInt(int64(off))
	l.SetInt(int64(length))
	return true
}

func encodeEntity(e tg.MessageEntityClass) (string, error) {
	var b bin.Buffer
	if err := e.Encode(&b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b.Buf), nil
}

func decodeEntity(s string) (tg.MessageEntityClass, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return tg.DecodeMessageEntity(&bin.Buffer{Buf: raw})
}

func encodeEntities(ents []tg.MessageEntityClass) []string {
	var out []string
	for _, e := range ents {
		if s, err := encodeEntity(e); err == nil {
			out = append(out, s)
		}
	}
	return out
}

func decodeEntities(in []string) []tg.MessageEntityClass {
	var out []tg.MessageEntityClass
	for _, s := range in {
		if e, err := decodeEntity(s); err == nil {
			out = append(out, e)
		}
	}
	return out
}
