package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
)

func TestTexts(t *testing.T) {
	var texts []string
	if err := json.Unmarshal(textsJSON, &texts); err != nil {
		t.Fatal(err)
	}
	if len(texts) < 100 {
		t.Fatalf("only %d texts", len(texts))
	}
	seen := map[string]bool{}
	for _, s := range texts {
		if strings.TrimSpace(s) == "" || len([]rune(s)) > 4000 || seen[s] {
			t.Fatalf("bad text %q", s)
		}
		seen[s] = true
	}
}

func TestDeckNoRepeat(t *testing.T) {
	d := newDeck([]string{"a", "b", "c"})
	prev := ""
	for round := 0; round < 50; round++ {
		got := map[string]bool{}
		for i := 0; i < 3; i++ {
			s := d.next()
			if s == prev {
				t.Fatalf("repeat %q", s)
			}
			prev = s
			got[s] = true
		}
		if len(got) != 3 {
			t.Fatalf("round %d not a full cycle: %v", round, got)
		}
	}
}

func TestRealReplyID(t *testing.T) {
	// Plain reply passes through.
	plain := &tg.Message{ReplyTo: &tg.MessageReplyHeader{ReplyToMsgID: 7}}
	if got := realReplyID(plain); got != 7 {
		t.Errorf("plain reply = %d, want 7", got)
	}
	// A message inside a forum topic that only carries the implicit
	// reply-to-topic-root header is not a reply.
	root := &tg.Message{ReplyTo: &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 3}}
	if got := realReplyID(root); got != 0 {
		t.Errorf("topic root header = %d, want 0", got)
	}
	// A real reply inside a topic keeps its target.
	inTopic := &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 9}
	inTopic.SetReplyToTopID(3)
	if got := realReplyID(&tg.Message{ReplyTo: inTopic}); got != 9 {
		t.Errorf("reply in topic = %d, want 9", got)
	}
	if got := realReplyID(&tg.Message{}); got != 0 {
		t.Errorf("no reply = %d, want 0", got)
	}
	if got := realReplyID(nil); got != 0 {
		t.Errorf("nil message = %d, want 0", got)
	}
}
