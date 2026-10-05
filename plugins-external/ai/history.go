package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const (
	historyFile = "history.json"
	// storeCap bounds messages kept per chat in memory and on disk.
	storeCap = 200
)

// historyStore keeps per-chat Q&A turns, persisted as one JSON file.
type historyStore struct {
	mu    sync.Mutex
	path  string
	chats map[int64][]turn
}

func newHistoryStore(dir string) *historyStore {
	return &historyStore{
		path:  filepath.Join(dir, historyFile),
		chats: make(map[int64][]turn),
	}
}

func (s *historyStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, &s.chats)
}

func (s *historyStore) saveLocked() error {
	b, err := json.MarshalIndent(s.chats, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tmp-*")
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
	return os.Rename(name, s.path)
}

// history returns the last turns Q&A pairs of a chat.
func (s *historyStore) history(chatID int64, turns int) []turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return trimTurns(s.chats[chatID], turns)
}

// appendUser records the user's question and returns the full history.
func (s *historyStore) appendUser(chatID int64, question string) []turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chats[chatID] = append(s.chats[chatID], turn{Role: roleUser, Text: question})
	if len(s.chats[chatID]) > storeCap {
		s.chats[chatID] = s.chats[chatID][len(s.chats[chatID])-storeCap:]
	}
	_ = s.saveLocked()
	return s.chats[chatID]
}

// appendAssistant records the reply.
func (s *historyStore) appendAssistant(chatID int64, answer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chats[chatID] = append(s.chats[chatID], turn{Role: roleAssistant, Text: answer})
	if len(s.chats[chatID]) > storeCap {
		s.chats[chatID] = s.chats[chatID][len(s.chats[chatID])-storeCap:]
	}
	_ = s.saveLocked()
}

// rollbackUser drops the trailing user turn after a failed request.
func (s *historyStore) rollbackUser(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.chats[chatID]
	if len(h) > 0 && h[len(h)-1].Role == roleUser {
		s.chats[chatID] = h[:len(h)-1]
	}
	_ = s.saveLocked()
}

// reset clears a chat's history and reports how many messages were dropped.
func (s *historyStore) reset(chatID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.chats[chatID])
	delete(s.chats, chatID)
	_ = s.saveLocked()
	return n
}

// trimTurns keeps the last turns*2 messages, aligned so the window starts on
// a user turn and a conversation is never cut mid-pair. turns<=0 returns nil.
func trimTurns(h []turn, turns int) []turn {
	if turns <= 0 || len(h) == 0 {
		return nil
	}
	limit := turns * 2
	if len(h) <= limit {
		return h
	}
	for limit < len(h) && h[len(h)-limit].Role != roleUser {
		limit++
	}
	return h[len(h)-limit:]
}
