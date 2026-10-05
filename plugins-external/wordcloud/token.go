package main

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Tokenizer ported strictly from TeleBox cy.ts: clean URLs/mentions, keep
// Han + [a-zA-Z0-9_+-.], then weigh English words, numbers and Han n-grams.

var stopWords = map[string]struct{}{
	"这个": {}, "那个": {}, "就是": {}, "不是": {}, "可以": {}, "没有": {}, "一下": {}, "一个": {},
	"什么": {}, "怎么": {}, "为什么": {}, "然后": {}, "现在": {}, "还是": {}, "但是": {}, "因为": {},
	"所以": {}, "如果": {}, "已经": {}, "应该": {}, "可能": {}, "感觉": {}, "不要": {}, "知道": {},
	"看看": {}, "哈哈": {}, "哈哈哈": {}, "你们": {}, "我们": {}, "他们": {}, "自己": {}, "直接": {},
	"确实": {}, "来源": {}, "情况": {}, "情况下": {}, "耗时": {}, "输入": {}, "输出": {}, "回复": {},
	"问题": {}, "最近": {}, "消息": {}, "有效": {}, "今天": {}, "昨天": {}, "明天": {}, "时候": {},
	"东西": {}, "里面": {}, "这里": {}, "那里": {}, "这样": {}, "那样": {}, "进行": {}, "使用": {},
	"需要": {}, "更新": {}, "主要": {}, "内容": {}, "新增": {}, "版本": {}, "发布": {}, "包括": {},
	"所有": {}, "不会": {},
	"the": {}, "and": {}, "for": {}, "with": {}, "this": {}, "that": {}, "you": {},
	"are": {}, "not": {}, "but": {}, "from": {}, "have": {},
	"http": {}, "https": {}, "com": {}, "www": {}, "telegram": {}, "t.me": {},
	"true": {}, "false": {}, "null": {}, "undefined": {},
}

var (
	reURL     = regexp.MustCompile(`(?i)https?://\S+`)
	reMention = regexp.MustCompile(`[@#][0-9A-Za-z_\x{4e00}-\x{9fa5}-]+`)
	reNoise   = regexp.MustCompile(`[^\p{Han}0-9A-Za-z_+\-.]+`)
	reEnglish = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9_+\-.]{1,24}`)
	reNumber  = regexp.MustCompile(`[0-9]{2,}[a-zA-Z%]?`)
	reHanRun  = regexp.MustCompile(`[\p{Han}]{2,}`)

	reOnlyDigits = regexp.MustCompile(`^[0-9]+$`)
	reOneTwoAbc  = regexp.MustCompile(`^[a-zA-Z]{1,2}$`)
	reOnlyO0     = regexp.MustCompile(`(?i)^[o0]+$`)
	reOnlySym    = regexp.MustCompile(`^[._+\-]+$`)
)

func isUsefulWord(word string) bool {
	if word == "" {
		return false
	}
	w := strings.ToLower(word)
	if _, ok := stopWords[w]; ok {
		return false
	}
	if reOnlyDigits.MatchString(w) || reOneTwoAbc.MatchString(w) ||
		reOnlyO0.MatchString(w) || reOnlySym.MatchString(w) {
		return false
	}
	return true
}

// wordCounts is an insertion-ordered counter (JS Map semantics: iteration
// order = first-seen order, which the sort in buildWordItems relies on).
type wordCounts struct {
	index   map[string]int
	entries []wordEntry
}

type wordEntry struct {
	word  string
	count int
}

func newWordCounts() *wordCounts {
	return &wordCounts{index: map[string]int{}}
}

func (c *wordCounts) add(word string, weight int) {
	w := strings.ToLower(strings.TrimSpace(word))
	if !isUsefulWord(w) {
		return
	}
	if i, ok := c.index[w]; ok {
		c.entries[i].count += weight
		return
	}
	c.index[w] = len(c.entries)
	c.entries = append(c.entries, wordEntry{word: w, count: weight})
}

func collectWords(text string, counts *wordCounts) {
	cleaned := reURL.ReplaceAllString(text, " ")
	cleaned = reMention.ReplaceAllString(cleaned, " ")
	cleaned = reNoise.ReplaceAllString(cleaned, " ")

	for _, m := range reEnglish.FindAllString(cleaned, -1) {
		counts.add(m, 2)
	}
	for _, m := range reNumber.FindAllString(cleaned, -1) {
		counts.add(m, 1)
	}
	for _, part := range reHanRun.FindAllString(cleaned, -1) {
		r := []rune(part)
		if len(r) <= 4 {
			counts.add(part, 3)
			continue
		}
		for size := 2; size <= 5; size++ {
			for i := 0; i+size <= len(r); i++ {
				edge := 0
				if i == 0 || i+size == len(r) {
					edge = 1
				}
				base := 1
				if size > 3 {
					base = 2
				}
				counts.add(string(r[i:i+size]), base+edge)
			}
		}
	}
}

// pruneOverlappingWords drops a word when a longer word contains it and is
// at least 0.9x as frequent (the short one is just a fragment).
func pruneOverlappingWords(entries []wordEntry) []wordEntry {
	// Candidates sorted by count desc: once counts fall below 0.9x of the
	// word's count nothing later can drop it, so we can break early.
	cand := make([]wordEntry, len(entries))
	copy(cand, entries)
	sort.SliceStable(cand, func(i, j int) bool { return cand[i].count > cand[j].count })

	lens := make([]int, len(entries))
	for i, e := range entries {
		lens[i] = len([]rune(e.word))
	}
	out := make([]wordEntry, 0, len(entries))
	for idx, e := range entries {
		if lens[idx] <= 1 {
			continue
		}
		threshold := 0.9 * float64(e.count)
		dropped := false
		for _, o := range cand {
			if float64(o.count) < threshold {
				break
			}
			if o.word == e.word || len([]rune(o.word)) <= lens[idx] || !strings.Contains(o.word, e.word) {
				continue
			}
			dropped = true
			break
		}
		if !dropped {
			out = append(out, e)
		}
	}
	return out
}

const (
	maxWords     = 220
	minFontSize  = 12
	fontSpan     = 68
	sizeExponent = 0.7
)

// wordItem is one cloud entry, positioned during layout.
type wordItem struct {
	word  string
	count int
	size  int
	color int // index into palette

	// placement (x, y = left edge / baseline in px)
	x, y, width, height float64
}

func buildWordItems(counts *wordCounts) []wordItem {
	entries := pruneOverlappingWords(counts.entries)
	kept := entries[:0]
	for _, e := range entries {
		if e.count >= 2 {
			kept = append(kept, e)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].count > kept[j].count })
	if len(kept) > maxWords {
		kept = kept[:maxWords]
	}
	if len(kept) == 0 {
		return nil
	}
	max, min := kept[0].count, kept[len(kept)-1].count
	spread := max - min
	if spread < 1 {
		spread = 1
	}
	items := make([]wordItem, 0, len(kept))
	for i, e := range kept {
		ratio := float64(e.count-min) / float64(spread)
		size := minFontSize + int(math.Round(math.Pow(ratio, sizeExponent)*fontSpan))
		items = append(items, wordItem{word: e.word, count: e.count, size: size, color: i % len(palette)})
	}
	return items
}
