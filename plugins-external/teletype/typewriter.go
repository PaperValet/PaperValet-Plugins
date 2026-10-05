package main

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	cursorChar = "█"
	// maxRunes caps one animation so a long text cannot turn into an
	// endless edit loop on the account.
	maxRunes = 200
	// baseInterval is the delay between frames used by the TeleBox source.
	baseInterval = 50 * time.Millisecond
	// minInterval keeps a mistyped speed setting from spamming edits.
	minInterval = 10 * time.Millisecond
	// maxFloodWait is the longest FLOOD_WAIT one animation will sleep out
	// before giving up (and closing the message out with the full text).
	maxFloodWait = 30 * time.Second
	// floodBudget caps the total number of flood-sleep retries per run so
	// a heavily throttled account cannot keep an animation alive forever.
	floodBudget = 6
)

// sleepCtx waits for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// frame is one visible state of the message during the animation.
type frame struct {
	text   string // content typed so far, raw and unescaped
	cursor bool   // show the block cursor after the content
}

// steps builds the typewriter frame sequence exactly like the TeleBox
// source: a lone cursor first, then for every character one frame with the
// cursor and one without. Iterating runes matches the source's per-code
// point loop (JS for..of).
func steps(text string) []frame {
	rs := []rune(text)
	out := make([]frame, 0, 1+2*len(rs))
	out = append(out, frame{cursor: true})
	buf := make([]rune, 0, len(rs))
	for _, r := range rs {
		buf = append(buf, r)
		s := string(buf)
		out = append(out, frame{text: s, cursor: true}, frame{text: s})
	}
	return out
}

// frameMD renders a frame as Telegram Markdown. The content is user text
// and must stay literal; the cursor character carries no Markdown meaning.
func frameMD(f frame) string {
	md := plugin.Escape(f.text)
	if f.cursor {
		md += cursorChar
	}
	return md
}

// tooLong reports whether text exceeds the per-run animation cap.
func tooLong(text string) bool { return utf8.RuneCountInString(text) > maxRunes }

// intervalMs converts a speed setting value ("25", "50", …) into a frame
// delay, falling back to the source default of 50ms for anything invalid.
func intervalMs(v string) time.Duration {
	def := int(baseInterval / time.Millisecond)
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		n = def
	}
	d := time.Duration(n) * time.Millisecond
	if d < minInterval {
		d = minInterval
	}
	return d
}

// autoSignal carries the slice of a MessageEvent the auto-mode decision
// needs, as a plain struct so the logic is testable without Telegram.
type autoSignal struct {
	edited bool
	out    bool
	media  bool
	text   string
}

// autoPick decides whether auto mode should animate a message, following
// the source: only the account's own plain text, never edits (our own
// animation edits come back as edits), never commands, and at least two
// characters.
func autoPick(s autoSignal, prefixes []string) bool {
	if s.edited || !s.out || s.media {
		return false
	}
	t := strings.TrimSpace(s.text)
	if t == "" || utf8.RuneCountInString(t) < 2 {
		return false
	}
	for _, p := range prefixes {
		if strings.HasPrefix(t, p) {
			return false
		}
	}
	return true
}
