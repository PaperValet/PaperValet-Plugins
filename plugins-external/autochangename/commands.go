package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tgerr"
	"github.com/robfig/cron/v3"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	pollEvery  = 20 * time.Second // wake-up granularity of the scheduler
	backoff    = 55 * time.Second // retry spacing after a failed rotation
	rpcTimeout = 30 * time.Second // per-RPC timeout
	listCap    = 60               // max items rendered in one message
)

func cronParse(spec string) (cron.Schedule, error) {
	return cron.ParseStandard(strings.TrimSpace(spec))
}

// currentTarget reads the panel target with a safe fallback.
func (p *ACNPlugin) currentTarget() string {
	t := p.set.String("target")
	if !validTarget(t) {
		return targetName
	}
	return t
}

// errText renders rotation errors bilingually.
func errText(tl func(string, string) string, err error) string {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "Flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	var e *tgerr.Error
	if errors.As(err, &e) {
		switch e.Type {
		case "USERNAME_INVALID", "USERNAME_PURCHASE_AVAILABLE":
			return tl("用户名不合法或已被占用", "The username is invalid or already taken")
		case "USERNAME_NOT_MODIFIED":
			return tl("资料没有变化", "Nothing to change")
		case "FIRSTNAME_INVALID":
			return tl("名字无效（不能为空或过长）", "Invalid name (empty or too long)")
		case "ABOUT_TOO_LONG":
			return tl("简介超过 70 字符限制", "The bio exceeds the 70-character limit")
		}
	}
	return tl("操作失败：", "Operation failed: ") + plugin.Escape(err.Error())
}

func (p *ACNPlugin) handle(ctx *plugin.CommandContext) error {
	sub := ""
	if len(ctx.Args) > 0 {
		sub = strings.ToLower(ctx.Args[0])
	}
	rest := ctx.RawArgs
	if len(ctx.Args) > 0 {
		rest = strings.TrimSpace(strings.TrimPrefix(ctx.RawArgs, ctx.Args[0]))
	}
	tl := ctx.Tlocal
	target := p.currentTarget()

	switch sub {
	case "", "help":
		return ctx.Edit(p.help(tl))
	case "list":
		return p.cmdList(ctx, target)
	case "add":
		return p.cmdAdd(ctx, target, rest)
	case "del":
		return p.cmdDel(ctx, target, rest)
	case "clear":
		return p.cmdClear(ctx, target)
	case "now":
		return p.cmdNow(ctx, target)
	case "status":
		return p.cmdStatus(ctx, target)
	case "restore":
		return p.cmdRestore(ctx)
	}
	return ctx.Edit("❌ " + fmt.Sprintf(tl("未知子命令：%s，发送 ", "Unknown subcommand: %s, send "), plugin.Code(sub)) + plugin.Code("autochangename help"))
}

func (p *ACNPlugin) help(tl func(string, string) string) string {
	line := func(cmd, zh, en string) string { return "> " + plugin.Code(cmd) + " " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("✏️ **" + tl("自动轮换名字/简介/用户名", "Auto name/bio/username rotation") + "**\n\n")
	b.WriteString("**" + tl("命令", "Commands") + "**\n")
	b.WriteString(line("autochangename list", "查看当前列表", "show the current list"))
	b.WriteString(line(tl("autochangename add 张三|李 · 或回复多行消息", "autochangename add John|Doe · or reply to a multiline message"),
		"添加条目（名字用 | 分隔姓和名；多条目请回复一条每行一条的消息用 add，或用面板添加）",
		"add entries (names use | to split first/last; for many entries reply to a one-per-line message with add, or use the panel)"))
	b.WriteString(line("autochangename del <序号>", "删除一条", "delete one entry"))
	b.WriteString(line("autochangename clear", "清空列表", "clear the list"))
	b.WriteString(line("autochangename now", "立即切换到下一条", "switch to the next entry now"))
	b.WriteString(line("autochangename status", "查看运行状态", "show the run status"))
	b.WriteString(line("autochangename restore", "恢复最初的资料", "restore the original profile"))
	b.WriteString("\n" + tl(
		"开关、轮换对象（名字/简介/用户名）、间隔、cron、随机顺序都在机器人面板里设置；列表也可以在面板里编辑。",
		"The toggle, target (name/bio/username), interval, cron and random order are set in the bot panel; the list can be edited there too.") + "\n")
	b.WriteString(tl("名字条目写法：", "Name entry format: ") + plugin.Code(tl("名|姓", "First|Last")) +
		tl("，只有名就只写名；", ", first name only is fine; ") + plugin.Code("|姓") + tl(" 表示清空姓。", " clears the last name."))
	return b.String()
}

func (p *ACNPlugin) cmdList(ctx *plugin.CommandContext, target string) error {
	tl := ctx.Tlocal
	items := p.st.items(target)
	title := "✏️ **" + tl("轮换列表", "Rotation list") + "** · " + targetLabel(tl, target)
	if len(items) == 0 {
		return ctx.Edit(title + "\n\n" + tl("列表为空，用 ", "The list is empty, use ") + plugin.Code("autochangename add") + tl(" 或面板添加。", " or the panel to add."))
	}
	cur := p.st.curIndex(target)
	var b strings.Builder
	b.WriteString(title + "  " + plugin.Code(len(items)) + "\n")
	for i, it := range items {
		if i >= listCap {
			b.WriteString("…\n")
			break
		}
		mark := "  "
		if i == cur {
			mark = "▶ "
		}
		b.WriteString(mark + strconv.Itoa(i+1) + ". " + plugin.Code(it) + "\n")
	}
	b.WriteString("\n" + statusFoot(tl, p, target))
	out := strings.TrimRight(b.String(), "\n")
	if len([]rune(out)) > 3900 {
		out = clipLines(out, 3900)
	}
	return ctx.Edit(out)
}

// clipLines truncates text to at most max runes without cutting a line in
// half (a cut mid-code-span breaks the Markdown rendering).
func clipLines(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := max
	for cut > 0 && r[cut-1] != '\n' {
		cut--
	}
	if cut == 0 {
		cut = max // single very long line: fall back to a hard cut
	}
	return strings.TrimRight(string(r[:cut]), "\n") + "\n…"
}

func (p *ACNPlugin) cmdAdd(ctx *plugin.CommandContext, target, text string) error {
	tl := ctx.Tlocal
	// The framework folds a pasted multiline command into one line, so
	// multiline entry works by replying to a message with the entries.
	if strings.TrimSpace(text) == "" {
		if m, err := ctx.ReplyMessage(); err == nil && strings.TrimSpace(m.Message) != "" {
			text = m.Message
		}
	}
	if strings.TrimSpace(text) == "" {
		return ctx.Edit("❌ " + tl("请提供要添加的内容；名字写法 ", "Give the entry to add; names use ") + plugin.Code("名|姓") +
			"\n" + tl("多条目：回复一条多行消息发 ", "Multiple entries: reply to a multiline message with ") + plugin.Code("autochangename add") +
			tl("（每行一条）", " (one per line)"))
	}
	existing := p.st.items(target)
	added, dup, invalid := parseItems(text, target, existing)
	if len(added) == 0 {
		var b strings.Builder
		b.WriteString("❌ " + tl("没有可添加的条目", "No entries to add"))
		if len(dup) > 0 {
			b.WriteString("\n⚠️ " + fmt.Sprintf(tl("跳过 %d 条重复", "Skipped %d duplicates"), len(dup)))
		}
		if len(invalid) > 0 {
			b.WriteString("\n❌ " + fmt.Sprintf(tl("跳过 %d 条无效", "Skipped %d invalid"), len(invalid)) + ": " + plugin.Code(firstFew(invalid)))
		}
		return ctx.Edit(b.String())
	}
	n, err := p.st.addItems(target, added, dup, invalid)
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	p.poke()
	var b strings.Builder
	b.WriteString("✅ " + fmt.Sprintf(tl("已添加 %d 条，共 %d 条", "Added %d entries, %d total"), len(added), n))
	if len(dup) > 0 {
		b.WriteString("\n⚠️ " + fmt.Sprintf(tl("跳过 %d 条重复", "Skipped %d duplicates"), len(dup)))
	}
	if len(invalid) > 0 {
		b.WriteString("\n❌ " + fmt.Sprintf(tl("跳过 %d 条无效", "Skipped %d invalid"), len(invalid)) + ": " + plugin.Code(firstFew(invalid)))
	}
	if target == targetUsername {
		b.WriteString("\n💡 " + tl("用户名轮换有 Telegram 限频，间隔别太短", "Username rotation is rate-limited by Telegram; keep the interval long"))
	}
	return ctx.Edit(b.String())
}

func (p *ACNPlugin) cmdDel(ctx *plugin.CommandContext, target, arg string) error {
	tl := ctx.Tlocal
	pos, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), "#")))
	if err != nil {
		return ctx.Edit("❌ " + tl("请输入有效的序号，如 ", "Give a valid index, e.g. ") + plugin.Code("autochangename del 1"))
	}
	removed, err := p.st.deleteItem(target, pos)
	if err != nil {
		return ctx.Edit("❌ " + tl("序号超出范围", "Index out of range"))
	}
	p.poke()
	return ctx.Edit("✅ " + tl("已删除: ", "Deleted: ") + plugin.Code(removed) + " · " + fmt.Sprintf(tl("剩余 %d 条", "%d left"), len(p.st.items(target))))
}

func (p *ACNPlugin) cmdClear(ctx *plugin.CommandContext, target string) error {
	tl := ctx.Tlocal
	if err := p.st.clear(target); err != nil {
		return ctx.Edit("❌ " + tl("清空失败: ", "Clear failed: ") + plugin.Escape(err.Error()))
	}
	p.poke()
	return ctx.Edit("✅ " + tl("列表已清空", "The list is cleared"))
}

func (p *ACNPlugin) cmdNow(ctx *plugin.CommandContext, target string) error {
	tl := ctx.Tlocal
	item, err := p.applyNow(ctx.Context(), target)
	if err != nil {
		return ctx.Edit("❌ " + errText(tl, err))
	}
	return ctx.Edit("✅ " + tl("已切换到: ", "Switched to: ") + plugin.Code(item))
}

func (p *ACNPlugin) cmdStatus(ctx *plugin.CommandContext, target string) error {
	tl := ctx.Tlocal
	items := p.st.items(target)
	var b strings.Builder
	b.WriteString("📊 **" + tl("自动轮换状态", "Auto-rotation status") + "**\n")
	if p.set.Bool("enabled") {
		b.WriteString(tl("轮换", "Rotation") + "  ✅ " + tl("运行中", "running") + "\n")
	} else {
		b.WriteString(tl("轮换", "Rotation") + "  ⏸ " + tl("已停用（面板里开启）", "disabled (enable in the panel)") + "\n")
	}
	b.WriteString(tl("对象", "Target") + "  " + plugin.Code(targetLabel(tl, target)) + "\n")
	b.WriteString(tl("条目", "Entries") + "  " + plugin.Code(len(items)))
	if len(items) > 0 {
		b.WriteString(" · " + tl("当前", "current") + " " + plugin.Code(truncateRunes(p.st.items(target)[p.st.curIndex(target)], 30)))
	}
	b.WriteString("\n" + scheduleText(tl, p.set.Int("interval"), p.set.String("cron"), p.set.Bool("random")))
	return ctx.Edit(b.String())
}

func (p *ACNPlugin) cmdRestore(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	if err := p.doRestore(ctx.Context()); err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "nothing captured") {
			return ctx.Edit("❌ " + tl("还没有保存过原始资料（先轮换一次）", "No original profile captured yet (rotate once first)"))
		}
		return ctx.Edit("❌ " + errText(tl, err))
	}
	return ctx.Edit("✅ " + tl("已恢复最初的资料", "The original profile is restored"))
}

// targetLabel names a target bilingually.
func targetLabel(tl func(string, string) string, target string) string {
	switch target {
	case targetBio:
		return tl("简介", "bio")
	case targetUsername:
		return tl("用户名", "username")
	}
	return tl("名字", "name")
}

// scheduleText renders the interval/cron/order summary line.
func scheduleText(tl func(string, string) string, intervalMin int, cronSpec string, random bool) string {
	order := tl("顺序", "order")
	if random {
		order += ": " + tl("随机", "random")
	} else {
		order += ": " + tl("顺序循环", "sequential")
	}
	if s := strings.TrimSpace(cronSpec); s != "" {
		return tl("计划", "Schedule") + "  " + plugin.Code(s) + " · " + order
	}
	m := intervalMin
	if m < 1 {
		m = 1
	}
	return tl("间隔", "Interval") + "  " + plugin.Code(fmt.Sprintf(tl("%d 分钟", "%d min"), m)) + " · " + order
}

// statusFoot is the one-line summary appended to list output.
func statusFoot(tl func(string, string) string, p *ACNPlugin, target string) string {
	return scheduleText(tl, p.set.Int("interval"), p.set.String("cron"), p.set.Bool("random")) +
		" · " + tl("对象", "target") + " " + plugin.Code(targetLabel(tl, target))
}

func firstFew(items []string) string {
	if len(items) > 3 {
		items = items[:3]
	}
	return strings.Join(items, ", ")
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
