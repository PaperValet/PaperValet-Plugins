package main

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const channelBase = 1000000000000 // PaperValet chat id of channel X is -(base+X)

// msgLink is one parsed message link.
type msgLink struct {
	Username  string // public chat username, if any
	ChannelID int64  // private channel id (t.me/c/<id>/...), raw form
	MsgID     int
}

func (l msgLink) sameChat(o msgLink) bool {
	return strings.EqualFold(l.Username, o.Username) && l.ChannelID == o.ChannelID
}

func (l msgLink) String() string {
	if l.Username != "" {
		return fmt.Sprintf("t.me/%s/%d", l.Username, l.MsgID)
	}
	return fmt.Sprintf("t.me/c/%d/%d", l.ChannelID, l.MsgID)
}

var (
	reUsername = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,31}$`)
	rePipe     = regexp.MustCompile(`\s*\|\s*`)
)

// isLink reports whether s looks like a Telegram message link.
func isLink(s string) bool {
	l := strings.ToLower(s)
	for _, p := range []string{"https://", "http://"} {
		l = strings.TrimPrefix(l, p)
	}
	return strings.HasPrefix(l, "t.me/") || strings.HasPrefix(l, "telegram.me/") ||
		strings.HasPrefix(l, "telegram.dog/") || strings.HasPrefix(l, "tg://privatepost") ||
		strings.HasPrefix(l, "tg://resolve")
}

// parseLink parses t.me/<user>/<id>, t.me/<user>/<topic>/<id>,
// t.me/c/<chan>/<id>, t.me/c/<chan>/<topic>/<id>, t.me/s/<user>/<id>,
// tg://resolve?domain=<user>&post=<id> and tg://privatepost?channel=<c>&post=<id>.
func parseLink(s string) (msgLink, error) {
	var l msgLink
	raw := strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(raw), "tg://") {
		u, err := url.Parse(raw)
		if err != nil {
			return l, fmt.Errorf("invalid link")
		}
		q := u.Query()
		id, err := strconv.Atoi(q.Get("post"))
		if err != nil || id <= 0 {
			return l, fmt.Errorf("missing message id")
		}
		l.MsgID = id
		switch strings.ToLower(u.Host) {
		case "resolve":
			l.Username = q.Get("domain")
			if !reUsername.MatchString(l.Username) {
				return l, fmt.Errorf("invalid username")
			}
		case "privatepost":
			c, err := strconv.ParseInt(q.Get("channel"), 10, 64)
			if err != nil || c <= 0 {
				return l, fmt.Errorf("invalid channel id")
			}
			l.ChannelID = c
		default:
			return l, fmt.Errorf("unsupported link")
		}
		return l, nil
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	low := strings.ToLower(raw)
	for _, p := range []string{"https://", "http://"} {
		if strings.HasPrefix(low, p) {
			raw, low = raw[len(p):], low[len(p):]
		}
	}
	for _, h := range []string{"t.me/", "telegram.me/", "telegram.dog/"} {
		if strings.HasPrefix(low, h) {
			raw = raw[len(h):]
			break
		}
	}
	parts := strings.Split(strings.Trim(raw, "/"), "/")
	if len(parts) > 0 && parts[0] == "s" {
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return l, fmt.Errorf("cannot parse link")
	}
	id, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || id <= 0 {
		return l, fmt.Errorf("missing message id")
	}
	l.MsgID = id
	if parts[0] == "c" {
		if len(parts) < 3 {
			return l, fmt.Errorf("cannot parse link")
		}
		c, err := strconv.ParseInt(strings.TrimPrefix(parts[1], "-100"), 10, 64)
		if err != nil || c <= 0 {
			return l, fmt.Errorf("invalid channel id")
		}
		l.ChannelID = c
		return l, nil
	}
	if !reUsername.MatchString(parts[0]) {
		return l, fmt.Errorf("invalid username")
	}
	l.Username = parts[0]
	return l, nil
}

// parsedArgs is the classification of the save command arguments.
type parsedArgs struct {
	Links      []msgLink
	Range      *[2]msgLink
	TempTarget string
	BadLink    string
	BadReason  string
}

// classifyArgs splits arguments into links, an optional "a|b" range and an
// optional trailing temporary target (reference behaviour). "a | b" with
// spaces is accepted as a range as well.
func classifyArgs(args []string) parsedArgs {
	var pa parsedArgs
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "|") {
		// Rebuild tokens so "a | b", "a |b", "a| b" all become "a|b".
		joined = rePipe.ReplaceAllString(joined, "|")
		args = strings.Fields(joined)
	}
	for i, a := range args {
		if strings.Contains(a, "|") && isLink(a) {
			ps := strings.SplitN(a, "|", 2)
			l1, e1 := parseLink(ps[0])
			l2, e2 := parseLink(ps[1])
			if e1 != nil || e2 != nil {
				pa.BadLink = a
				if e1 != nil {
					pa.BadReason = e1.Error()
				} else {
					pa.BadReason = e2.Error()
				}
				return pa
			}
			if !l1.sameChat(l2) {
				pa.BadLink, pa.BadReason = a, "range links must be in the same chat"
				return pa
			}
			pa.Range = &[2]msgLink{l1, l2}
			if i == len(args)-2 && !isLink(args[i+1]) {
				pa.TempTarget = args[i+1]
			}
			return pa
		}
		if isLink(a) {
			l, err := parseLink(a)
			if err != nil {
				pa.BadLink, pa.BadReason = a, err.Error()
				return pa
			}
			pa.Links = append(pa.Links, l)
			continue
		}
		if i == len(args)-1 && len(pa.Links) > 0 {
			pa.TempTarget = a
		}
	}
	return pa
}

// channelChatID converts a raw channel id to the PaperValet chat id.
func channelChatID(id int64) int64 { return -(channelBase + id) }

// messageLink builds a link to a message; "" when none is possible
// (private chats and basic groups have no message links).
func messageLink(chatID int64, username string, msgID int) string {
	if username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", username, msgID)
	}
	if chatID <= -channelBase {
		return fmt.Sprintf("https://t.me/c/%d/%d", -chatID-channelBase, msgID)
	}
	return ""
}

type idRange struct{ Start, End int }

// compactIDs merges sorted unique ids into consecutive ranges.
func compactIDs(ids []int) []idRange {
	if len(ids) == 0 {
		return nil
	}
	s := append([]int(nil), ids...)
	sort.Ints(s)
	var out []idRange
	cur := idRange{s[0], s[0]}
	for _, id := range s[1:] {
		switch {
		case id == cur.End:
		case id == cur.End+1:
			cur.End = id
		default:
			out = append(out, cur)
			cur = idRange{id, id}
		}
	}
	return append(out, cur)
}

// sanitizeSegment mirrors the reference path sanitizer.
func sanitizeSegment(v, fallback string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.TrimSpace(v) {
		ok := r == '_' || r == '-' || r == '.' || (r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r))) ||
			(r >= 0x4e00 && r <= 0x9fff)
		if !ok || r == '_' {
			if !lastUnderscore {
				b.WriteByte('_')
			}
			lastUnderscore = true
			continue
		}
		b.WriteRune(r)
		lastUnderscore = false
	}
	s := strings.Trim(b.String(), "_")
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	if s == "" || s == "." || s == ".." {
		return fallback
	}
	return s
}

var mimeExt = []struct{ sub, ext string }{
	{"video/mp4", ".mp4"}, {"video/webm", ".webm"}, {"video/quicktime", ".mov"},
	{"audio/mpeg", ".mp3"}, {"audio/ogg", ".ogg"}, {"audio/mp4", ".m4a"}, {"audio/x-flac", ".flac"}, {"audio/flac", ".flac"},
	{"image/jpeg", ".jpg"}, {"image/jpg", ".jpg"}, {"image/png", ".png"}, {"image/gif", ".gif"},
	{"image/webp", ".webp"}, {"application/x-tgsticker", ".tgs"}, {"application/pdf", ".pdf"},
	{"application/zip", ".zip"},
}

// extFor picks a file extension from the original file name, then mime type.
func extFor(fileName, mime, kind string) string {
	if e := strings.ToLower(filepath.Ext(fileName)); e != "" && len(e) <= 10 {
		return e
	}
	m := strings.ToLower(mime)
	for _, x := range mimeExt {
		if strings.Contains(m, x.sub) {
			return x.ext
		}
	}
	switch kind {
	case "photo":
		return ".jpg"
	case "video", "gif":
		return ".mp4"
	case "voice":
		return ".ogg"
	case "audio":
		return ".mp3"
	case "sticker":
		return ".webp"
	}
	return ".bin"
}
