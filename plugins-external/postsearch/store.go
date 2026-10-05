package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// config mirrors the TeleBox temp/channel_search_config.json layout.
type config struct {
	DefaultChannel string    `json:"defaultChannel"`
	Channels       []channel `json:"channelList"`
	AdFilters      []string  `json:"adFilters"`
}

// channel is one searchable source.
type channel struct {
	Title       string `json:"title"`
	Handle      string `json:"handle"`
	LinkedGroup string `json:"linkedGroup,omitempty"`
	Username    string `json:"username,omitempty"` // canonical @name for links
	ChatID      int64  `json:"chatId,omitempty"`   // -100… form for links
}

func (c *channel) label() string {
	if c.Title != "" {
		return c.Title
	}
	return c.Handle
}

// link returns a t.me message link, or "" when the channel has no known
// reference (no username and no chat id).
func (c *channel) link(msgID int) string {
	base := ""
	if c.Username != "" {
		base = "https://t.me/" + strings.TrimPrefix(c.Username, "@") + "/"
	} else if c.ChatID != 0 {
		// -1001234… → internal id 1234…
		raw := -c.ChatID - 1000000000000
		if raw > 0 {
			base = "https://t.me/c/" + strconv.FormatInt(raw, 10) + "/"
		}
	}
	if base == "" || msgID <= 0 {
		return ""
	}
	return base + strconv.Itoa(msgID)
}

// linkForMsg renders a link to any message found in a search, using the
// channel entry when the message's own chat matches it, else the chat
// reference carried by the search result.
func (c *channel) linkForMsg(chatRef chatRef, msgID int) string {
	if l := c.link(msgID); l != "" {
		return l
	}
	return chatRef.link(msgID)
}

// defaultAdFilters is the source's built-in Chinese ad keyword list.
var defaultAdFilters = []string{
	"广告", "推广", "赞助", "合作", "代理", "招商", "加盟", "投资", "理财",
	"贷款", "借钱", "网贷", "信用卡", "pos机", "刷单", "兼职", "副业",
	"微商", "代购", "淘宝", "拼多多", "京东", "直播带货", "优惠券",
	"返利", "红包", "现金", "提现", "充值", "游戏币", "点卡",
	"彩票", "博彩", "赌博", "六合彩", "时时彩", "北京赛车",
	"股票", "期货", "外汇", "数字货币", "比特币", "挖矿",
	"保险", "医疗", "整容", "减肥", "丰胸", "壮阳", "药品",
	"假货", "高仿", "A货", "精仿", "原单", "尾单",
	"办证", "刻章", "发票", "学历", "文凭", "证书",
	"黑客", "破解", "外挂", "木马", "病毒", "盗号",
	"vpn", "翻墙", "代理ip", "科学上网", "梯子",
}

// store persists the config in data/postsearch/config.json.
type store struct {
	mu   sync.Mutex
	dir  string
	cfg  config
	host plugin.Host
}

func newStore(host plugin.Host) *store {
	return &store{host: host, cfg: config{AdFilters: append([]string(nil), defaultAdFilters...)}}
}

func (s *store) path() string {
	if s.dir == "" {
		return "config.json"
	}
	return filepath.Join(s.dir, "config.json")
}

// load reads the persisted config, keeping defaults for missing fields.
func (s *store) load() error {
	if s.host == nil {
		return nil
	}
	dir, err := s.host.DataDir("postsearch")
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.dir = dir
	s.mu.Unlock()
	b, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.saveLocked()
		}
		return err
	}
	var cfg config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	if cfg.AdFilters == nil {
		cfg.AdFilters = append([]string(nil), defaultAdFilters...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	return nil
}

// saveLocked writes atomically; caller holds mu.
func (s *store) saveLocked() error {
	if s.dir == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".config-*")
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
	return os.Rename(name, s.path())
}

func (s *store) withLock(fn func(cfg *config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.cfg); err != nil {
		return err
	}
	return s.saveLocked()
}

func (s *store) snapshot() config {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := config{DefaultChannel: s.cfg.DefaultChannel}
	out.Channels = append([]channel(nil), s.cfg.Channels...)
	out.AdFilters = append([]string(nil), s.cfg.AdFilters...)
	return out
}

// searchOrder lists handles default-first without duplicates.
func (c *config) searchOrder() []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	add(c.DefaultChannel)
	for _, ch := range c.Channels {
		add(ch.Handle)
	}
	return out
}

func (c *config) byHandle(h string) (channel, bool) {
	for _, ch := range c.Channels {
		if ch.Handle == h {
			return ch, true
		}
	}
	return channel{}, false
}

// removeChannels deletes by handles (and keeps default sane), returning
// the removed entries in order.
func (c *config) removeChannels(handles map[string]bool) []channel {
	var removed []channel
	var kept []channel
	for _, ch := range c.Channels {
		if handles[ch.Handle] {
			removed = append(removed, ch)
			continue
		}
		kept = append(kept, ch)
	}
	if len(removed) > 0 {
		c.Channels = kept
		if c.DefaultChannel != "" && handles[c.DefaultChannel] {
			if len(kept) > 0 {
				c.DefaultChannel = kept[0].Handle
			} else {
				c.DefaultChannel = ""
			}
		}
	}
	return removed
}

// expandIndexes maps 1-based index tokens to handles; invalid tokens come
// back as-is so callers can report them.
func expandIndexes(c *config, tokens []string) map[string]bool {
	handles := map[string]bool{}
	for _, t := range tokens {
		if n, ok := atoiPositive(t); ok && n <= len(c.Channels) {
			handles[c.Channels[n-1].Handle] = true
		} else {
			handles[t] = true
		}
	}
	return handles
}

func atoiPositive(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return 0, false
		}
	}
	return n, true
}
