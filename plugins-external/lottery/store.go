package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

const storeFile = "state.json"

// db is the persisted state of the plugin.
type db struct {
	NextID     int64                  `json:"next_id"`
	Lotteries  []*lottery             `json:"lotteries"`
	Warehouses map[string][]prizeItem `json:"warehouses"`
}

// prizeItem is one prize kind in a warehouse with stock.
type prizeItem struct {
	Text      string `json:"text"`
	Stock     int    `json:"stock"`
	Order     int    `json:"order"`
	CreatedAt int64  `json:"created_at"`
}

// store persists db to data/lottery/state.json atomically.
type store struct {
	mu   sync.Mutex
	path string
	data db
}

func newStore(dir string) *store {
	return &store{path: filepath.Join(dir, storeFile)}
}

func (s *store) load() error {
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
	if err := json.Unmarshal(b, &s.data); err != nil {
		return err
	}
	if s.data.Warehouses == nil {
		s.data.Warehouses = map[string][]prizeItem{}
	}
	return nil
}

// save writes the current state atomically. Must be called with s.mu held
// by the plugin lock too (single writer); store keeps its own lock for the
// file operations used by tests.
func (s *store) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked()
}

func (s *store) writeLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(&s.data, "", "  ")
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

// The plugin serializes every mutation under p.mu and copies what it needs.
// The helpers below run on the plugin side; the store only holds data.

func (p *LotteryPlugin) activeLocked(chatID int64) *lottery {
	for i := len(p.store.data.Lotteries) - 1; i >= 0; i-- {
		l := p.store.data.Lotteries[i]
		if l.ChatID == chatID && l.Status == "active" {
			return l
		}
	}
	return nil
}

func (p *LotteryPlugin) findLocked(id int64) *lottery {
	for _, l := range p.store.data.Lotteries {
		if l.ID == id {
			return l
		}
	}
	return nil
}

// createWarehouse adds a warehouse if new; reports whether it was created.
func (p *LotteryPlugin) createWarehouseLocked(name string) bool {
	if name == "" {
		return false
	}
	if _, ok := p.store.data.Warehouses[name]; ok {
		return false
	}
	p.store.data.Warehouses[name] = []prizeItem{}
	return true
}

// addPrizeLocked adds stock to an existing prize kind or appends a new one.
func (p *LotteryPlugin) addPrizeLocked(warehouse, text string, stock int) {
	items := p.store.data.Warehouses[warehouse]
	for i := range items {
		if items[i].Text == text {
			items[i].Stock += stock
			p.store.data.Warehouses[warehouse] = items
			return
		}
	}
	maxOrder := 0
	for _, it := range items {
		if it.Order > maxOrder {
			maxOrder = it.Order
		}
	}
	items = append(items, prizeItem{Text: text, Stock: stock, Order: maxOrder + 1, CreatedAt: time.Now().Unix()})
	p.store.data.Warehouses[warehouse] = items
}

// warehouseNames returns warehouse names sorted, as in the source.
func (p *LotteryPlugin) warehouseNamesLocked() []string {
	names := make([]string, 0, len(p.store.data.Warehouses))
	for n := range p.store.data.Warehouses {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// warehouseByNameOrIndex resolves a name or 1-based index to a name.
func warehouseByNameOrIndex(id string, names []string) (string, bool) {
	if n, err := strconv.Atoi(id); err == nil && n >= 1 && n <= len(names) {
		return names[n-1], true
	}
	for _, n := range names {
		if n == id {
			return n, true
		}
	}
	return "", false
}

// prizesInStock returns the warehouse items that still have stock, in order.
func (p *LotteryPlugin) prizesInStockLocked(warehouse string) []prizeItem {
	var out []prizeItem
	for _, it := range p.store.data.Warehouses[warehouse] {
		if it.Stock > 0 {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// nextPrizeLocked returns the first prize with stock and consumes one unit.
func (p *LotteryPlugin) nextPrizeLocked(warehouse string) (string, bool) {
	items := p.prizesInStockLocked(warehouse)
	if len(items) == 0 {
		return "", false
	}
	name := items[0].Text
	list := p.store.data.Warehouses[warehouse]
	for i := range list {
		if list[i].Text == name && list[i].Stock > 0 {
			list[i].Stock--
			break
		}
	}
	return name, true
}

// clearWarehouse removes one warehouse; returns the prize kinds removed.
func (p *LotteryPlugin) clearWarehouseLocked(name string) int {
	n := len(p.store.data.Warehouses[name])
	delete(p.store.data.Warehouses, name)
	return n
}

// expireClaimsLocked marks pending prizes past their deadline expired.
func (p *LotteryPlugin) expireClaimsLocked(now time.Time) int {
	n := 0
	for _, l := range p.store.data.Lotteries {
		for i := range l.Winners {
			w := &l.Winners[i]
			if w.Status == "pending" && w.ExpiresAt > 0 && w.ExpiresAt < now.Unix() {
				w.Status = "expired"
				n++
			}
		}
	}
	return n
}

// join adds a participant unless full or duplicate; reports the new count.
func (p *LotteryPlugin) joinLocked(l *lottery, u participant, now time.Time) (count int, err joinErr) {
	for _, ex := range l.Participants {
		if ex.UserID == u.UserID {
			return len(l.Participants), errDuplicate
		}
	}
	if len(l.Participants) >= l.MaxUsers {
		return len(l.Participants), errFull
	}
	u.JoinedAt = now.Unix()
	l.Participants = append(l.Participants, u)
	return len(l.Participants), joinOK
}

type joinErr int

const (
	joinOK joinErr = iota
	errDuplicate
	errFull
)

func (e joinErr) Error() string {
	switch e {
	case errDuplicate:
		return "duplicate participant"
	case errFull:
		return "lottery full"
	}
	return ""
}

// drawWinners picks n distinct winners from participants using a
// crypto/rand Fisher-Yates shuffle. Returns the winners, or fewer when
// there are not enough participants.
func drawWinners(parts []participant, n int) []participant {
	if n > len(parts) {
		n = len(parts)
	}
	if n <= 0 {
		return nil
	}
	shuffled := append([]participant(nil), parts...)
	for i := len(shuffled) - 1; i > 0; i-- {
		j := randIntn(i + 1)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	return shuffled[:n]
}
