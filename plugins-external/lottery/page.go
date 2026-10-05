package main

import (
	"fmt"
	"strings"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// page renders active lotteries and prize warehouses in the bot panel, with
// a draw/delete button per lottery.
func (p *LotteryPlugin) page(c *plugin.BotContext) (*plugin.View, error) {
	tl := c.Tlocal
	if op, rest, ok := strings.Cut(c.Data, ":"); ok {
		var id int64
		if _, err := fmt.Sscan(rest, &id); err == nil && id > 0 {
			switch op {
			case "draw":
				p.drawAsync(id, "panel")
			case "del":
				p.mu.Lock()
				out := p.store.data.Lotteries[:0]
				for _, x := range p.store.data.Lotteries {
					if x.ID != id {
						out = append(out, x)
					}
				}
				p.store.data.Lotteries = out
				p.saveLocked()
				p.mu.Unlock()
				c.Toast(tl("已删除", "Deleted"))
			}
		}
	}

	p.mu.Lock()
	var actives []*lottery
	for _, l := range p.store.data.Lotteries {
		if l.Status == "active" {
			actives = append(actives, l)
		}
	}
	names := p.warehouseNamesLocked()
	stocks := map[string]int{}
	for _, n := range names {
		for _, it := range p.prizesInStockLocked(n) {
			stocks[n] += it.Stock
		}
	}
	p.mu.Unlock()

	v := &plugin.View{Text: "🎰 **" + tl("抽奖", "Lotteries") + "**"}
	if len(actives) == 0 && len(names) == 0 {
		v.Text += "\n\n" + tl("暂无进行中的抽奖。在群里用 ", "No running lotteries. In a group, create one with ") + "`lottery create …`"
		return v, nil
	}
	for _, l := range actives {
		v.Text += fmt.Sprintf("\n\n**#%d** %s\n", l.ID, plugin.Escape(l.Title)) +
			fmt.Sprintf("%s  `%d/%d` · %s `%d`\n", tl("进度", "Progress"), len(l.Participants), l.MaxUsers, tl("中奖", "Winners"), l.WinnerCnt) +
			tl("关键词", "Keyword") + "  " + plugin.Code(l.Keyword)
		v.Buttons = append(v.Buttons, plugin.Row(
			plugin.Btn("🎁 "+tl("开奖", "Draw"), fmt.Sprintf("draw:%d", l.ID)).Primary(),
			plugin.Btn("🗑", fmt.Sprintf("del:%d", l.ID)).Danger(),
		))
	}
	if len(names) > 0 {
		v.Text += "\n\n**" + tl("奖品仓库", "Warehouses") + "**\n"
		for _, n := range names {
			v.Text += fmt.Sprintf("• %s — %s %d\n", plugin.Code(n), tl("库存", "stock"), stocks[n])
		}
	}
	return v, nil
}
