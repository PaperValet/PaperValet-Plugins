package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// page renders the rotation list with per-item delete buttons plus
// add / rotate-now / clear / restore actions. Data values:
//
//	""         open
//	"add"      ask for new entries
//	"new"      the typed entries (Ask reply)
//	"rm:<i>"   delete entry i (1-based)
//	"now"      rotate now
//	"clear"    clear the list
//	"restore"  restore the original profile
func (p *ACNPlugin) page(c *plugin.BotContext) (*plugin.View, error) {
	tl := c.Tlocal
	target := p.currentTarget()
	ctx := c.Context()

	switch {
	case c.Data == "add":
		c.Ask("new")
		return &plugin.View{Text: "✏️ " + tl(
			"发送要添加的条目，每行一条。名字写法 `名|姓`，简介/用户名直接写。",
			"Send the entries to add, one per line. Names use `First|Last`; bio/username as plain text.")}, nil
	case c.Data == "new":
		existing := p.st.items(target)
		added, dup, invalid := parseItems(c.Input, target, existing)
		if len(added) > 0 {
			if _, err := p.st.addItems(target, added, dup, invalid); err != nil {
				c.Alert("❌ " + plugin.Escape(err.Error()))
			} else {
				p.poke()
				c.Toast(fmt.Sprintf(tl("已添加 %d 条", "Added %d"), len(added)))
			}
		} else if len(dup)+len(invalid) > 0 {
			c.Alert(tl("没有新增：全部重复或无效", "Nothing added: all duplicate or invalid"))
		}
	case strings.HasPrefix(c.Data, "rm:"):
		if i, err := strconv.Atoi(strings.TrimPrefix(c.Data, "rm:")); err == nil {
			if removed, err := p.st.deleteItem(target, i); err == nil {
				p.poke()
				c.Toast(tl("已删除 ", "Deleted ") + truncateRunes(removed, 20))
			}
		}
	case c.Data == "now":
		if item, err := p.applyNow(ctx, target); err != nil {
			c.Alert("❌ " + errText(tl, err))
		} else {
			c.Toast(tl("已切换到 ", "Switched to ") + truncateRunes(item, 20))
		}
	case c.Data == "clear":
		if err := p.st.clear(target); err == nil {
			p.poke()
			c.Toast(tl("列表已清空", "List cleared"))
		}
	case c.Data == "restore":
		if err := p.doRestore(ctx); err != nil {
			c.Alert("❌ " + errText(tl, err))
		} else {
			c.Toast(tl("已恢复原始资料", "Original profile restored"))
		}
	}

	return p.renderPage(ctx, c, target), nil
}

func (p *ACNPlugin) renderPage(_ context.Context, c *plugin.BotContext, target string) *plugin.View {
	tl := c.Tlocal
	items := p.st.items(target)
	cur := p.st.curIndex(target)

	var b strings.Builder
	b.WriteString("✏️ **" + tl("轮换列表", "Rotation list") + "** · " + plugin.Code(targetLabel(tl, target)) + "  " + plugin.Code(len(items)) + "\n")
	if p.set.Bool("enabled") {
		b.WriteString("✅ " + tl("轮换运行中", "rotation running"))
	} else {
		b.WriteString("⏸ " + tl("轮换已停用（在设置里开启）", "rotation disabled (enable in settings)"))
	}
	b.WriteString("\n" + scheduleText(tl, p.set.Int("interval"), p.set.String("cron"), p.set.Bool("random")) + "\n")
	if len(items) == 0 {
		b.WriteString("\n" + tl("列表为空，点 ➕ 添加。", "The list is empty; tap ➕ to add."))
	} else {
		b.WriteString("\n")
		for i, it := range items {
			if i >= listCap {
				b.WriteString("…\n")
				break
			}
			mark := ""
			if i == cur {
				mark = "▶ "
			}
			b.WriteString(mark + strconv.Itoa(i+1) + ". " + plugin.Code(truncateRunes(it, 40)) + "\n")
		}
	}
	v := &plugin.View{Text: strings.TrimRight(b.String(), "\n")}

	// Delete buttons, 4 per row, capped so the keyboard stays small.
	var row []plugin.Button
	for i := range items {
		if i >= 40 {
			break
		}
		row = append(row, plugin.Btn("🗑 "+strconv.Itoa(i+1), "rm:"+strconv.Itoa(i+1)).Danger())
		if len(row) == 4 {
			v.Buttons = append(v.Buttons, row)
			row = nil
		}
	}
	if len(row) > 0 {
		v.Buttons = append(v.Buttons, row)
	}
	actions := plugin.Row(plugin.Btn("➕ "+tl("添加", "Add"), "add").Success())
	if len(items) > 0 {
		actions = append(actions, plugin.Btn("🔄 "+tl("立即切换", "Rotate now"), "now").Primary())
	}
	v.Buttons = append(v.Buttons, actions)
	tail := plugin.Row(plugin.Btn("↩️ "+tl("恢复原名", "Restore"), "restore"))
	if len(items) > 0 {
		tail = append(tail, plugin.Btn("🧹 "+tl("清空", "Clear"), "clear").Danger())
	}
	v.Buttons = append(v.Buttons, tail)
	return v
}
