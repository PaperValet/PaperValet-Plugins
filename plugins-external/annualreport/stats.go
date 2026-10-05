package main

import (
	"sort"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// sample is the minimal message shape aggregation works on, so tests can
// feed fake slices without touching the network.
type sample struct {
	Date   int64
	Sender int64
	Bytes  int64
	Kind   string // text, photo, video, gif, audio, voice, sticker, document, other
}

// senderCount pairs a user with its message count.
type senderCount struct {
	ID int64
	N  int
}

// Report is the aggregated statistics of one chat and period.
type Report struct {
	ChatID   int64
	Period   string
	Title    string
	Messages int
	Scanned  int
	Senders  int
	Chars    int
	Media    int
	Days     int // distinct calendar days with messages
	Hours    [24]int
	Weekdays [7]int
	Top      []senderCount
	MediaMix map[string]int
	Bytes    int64
	First    int64
	Last     int64
	Names    map[int64]string
}

// toSamples converts fetched messages into aggregation input.
func toSamples(msgs []*tg.Message, names map[int64]string) []sample {
	out := make([]sample, 0, len(msgs))
	for _, m := range msgs {
		s := sample{Date: int64(m.Date), Sender: plugin.SenderID(m)}
		kind, size := mediaKind(m)
		s.Kind = kind
		s.Bytes = size
		out = append(out, s)
	}
	_ = names
	return out
}

// mediaKind classifies a message's media.
func mediaKind(m *tg.Message) (string, int64) {
	switch v := m.Media.(type) {
	case *tg.MessageMediaPhoto:
		return "photo", 0
	case *tg.MessageMediaDocument:
		d, ok := v.Document.(*tg.Document)
		if !ok {
			return "document", 0
		}
		return documentKind(d), d.Size
	case *tg.MessageMediaWebPage:
		return "text", 0
	default:
		if m.Message != "" {
			return "text", 0
		}
		return "other", 0
	}
}

// documentKind refines a document by its attributes, stickers first.
func documentKind(d *tg.Document) string {
	sticker := false
	for _, a := range d.Attributes {
		switch at := a.(type) {
		case *tg.DocumentAttributeSticker:
			_ = at
			sticker = true
		case *tg.DocumentAttributeVideo:
			if at.RoundMessage {
				return "video"
			}
			return "video"
		case *tg.DocumentAttributeAudio:
			if at.Voice {
				return "voice"
			}
			return "audio"
		case *tg.DocumentAttributeAnimated:
			return "gif"
		}
	}
	if sticker {
		return "sticker"
	}
	if d.MimeType == "image/webp" {
		return "sticker"
	}
	return "document"
}

// aggregate folds samples into a Report. from/to bound the period (zero =
// unbounded "all"); samples outside it are ignored.
func aggregate(chatID int64, label string, samples []sample, from, to time.Time) *Report {
	r := &Report{
		ChatID:   chatID,
		Period:   label,
		MediaMix: map[string]int{},
		Names:    map[int64]string{},
	}
	perSender := map[int64]int{}
	days := map[string]bool{}
	for _, s := range samples {
		t := time.Unix(s.Date, 0)
		if !from.IsZero() && t.Before(from) {
			continue
		}
		if !to.IsZero() && !t.Before(to) {
			continue
		}
		r.Messages++
		r.Scanned++
		r.Hours[t.Hour()]++
		r.Weekdays[t.Weekday()]++
		days[t.Format("2006-01-02")] = true
		if s.Sender != 0 {
			perSender[s.Sender]++
		}
		if s.Kind != "text" {
			r.Media++
			r.MediaMix[s.Kind]++
		}
		r.Bytes += s.Bytes
		if r.First == 0 || s.Date < r.First {
			r.First = s.Date
		}
		if s.Date > r.Last {
			r.Last = s.Date
		}
	}
	r.Days = len(days)
	r.Senders = len(perSender)
	for id, n := range perSender {
		r.Top = append(r.Top, senderCount{ID: id, N: n})
	}
	sort.Slice(r.Top, func(i, j int) bool {
		if r.Top[i].N != r.Top[j].N {
			return r.Top[i].N > r.Top[j].N
		}
		return r.Top[i].ID < r.Top[j].ID
	})
	if len(r.Top) > topN {
		r.Top = r.Top[:topN]
	}
	return r
}

const topN = 10

// fillChars adds text length stats (kept out of aggregate for testing).
func (r *Report) fillChars(msgs []*tg.Message) {
	for _, m := range msgs {
		r.Chars += len([]rune(m.Message))
	}
}

// hourBar renders one bar of the hour histogram.
func hourBar(n, max int) string {
	if max <= 0 {
		return ""
	}
	cells := n * barWidth / max
	if n > 0 && cells == 0 {
		cells = 1
	}
	return strings.Repeat("▇", cells)
}

const barWidth = 8
