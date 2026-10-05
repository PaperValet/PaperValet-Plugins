package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// handle dispatches lottery subcommands.
func (p *LotteryPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	sub := strings.ToLower(ctx.GetArg(0))
	if sub == "" {
		return ctx.Edit("❌ " + ctx.Tlocal("请指定子命令，使用 ", "Specify a subcommand, see ") + plugin.Code("lottery help"))
	}
	if sub == "help" {
		return ctx.Edit(p.help(ctx))
	}
	switch sub {
	case "create":
		return p.cmdCreate(ctx)
	case "draw":
		return p.cmdDraw(ctx)
	case "delete":
		return p.cmdDelete(ctx)
	case "status":
		return p.cmdStatus(ctx)
	case "list":
		return p.cmdList(ctx)
	case "winners":
		return p.cmdWinners(ctx)
	case "claim":
		return p.cmdClaim(ctx)
	case "prize":
		return p.cmdPrize(ctx)
	}
	return ctx.Edit("❌ " + ctx.Tlocal("未知子命令: ", "Unknown subcommand: ") + plugin.Code(sub) +
		"\n\n💡 " + plugin.Code("lottery help"))
}

func (p *LotteryPlugin) help(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	line := func(cmd, zh, en string) string { return plugin.Code(cmd) + "  " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("🎰 **" + tl("群抽奖", "Group Lottery") + "**\n\n")
	b.WriteString("**" + tl("流程", "Flow") + "**\n")
	b.WriteString(tl("先建奖品仓库 → 添加奖品 → 在群里创建抽奖 → 成员发送关键词报名 → 人满自动开奖或手动开奖\n\n",
		"Create a prize warehouse → add prizes → create the lottery in the group → members join by sending the keyword → auto-draw when full or draw manually\n\n"))
	b.WriteString("**" + tl("抽奖管理", "Lottery") + "**\n")
	b.WriteString(line("lottery create <标题> <关键词> <人数> <中奖数> [仓库] [at 时间|notify]",
		"创建抽奖并置顶公告（notify=置顶时提醒所有人，at=定时开奖，如 at 21:00）",
		"create and pin the announcement (notify=notify everyone when pinning, at=scheduled draw, e.g. at 21:00)"))
	b.WriteString(line("lottery draw", "手动开奖（创建者或群管理员）", "manual draw (creator or group admin)"))
	b.WriteString(line("lottery status", "查看当前抽奖进度", "show current lottery progress"))
	b.WriteString(line("lottery list", "查看参与名单", "list participants"))
	b.WriteString(line("lottery winners", "查看中奖名单与领奖状态", "list winners and claim status"))
	b.WriteString(line("lottery claim <ID/@用户名>", "标记中奖用户已领奖", "mark a winner's prize delivered"))
	b.WriteString(line("lottery delete", "删除本群进行中的抽奖", "delete this chat's running lottery"))
	b.WriteString("\n**" + tl("奖品仓库", "Prize warehouses") + "**\n")
	b.WriteString(line("lottery prize create <仓库名>", "创建仓库", "create a warehouse"))
	b.WriteString(line("lottery prize add <仓库> <\"奖品\"> <数量>", "添加奖品（同奖品自动累加库存）", "add a prize (same text adds stock)"))
	b.WriteString(line("lottery prize list [仓库]", "查看仓库与奖品", "list warehouses and prizes"))
	b.WriteString(line("lottery prize clear <仓库|all>", "清空仓库", "clear a warehouse or all"))
	b.WriteString("\n**" + tl("规则", "Rules") + "**\n")
	b.WriteString("• " + tl("每个群同时只有一个进行中的抽奖", "One active lottery per chat") + "\n")
	b.WriteString("• " + tl("关键词精确匹配，区分大小写", "Keyword must match exactly, case-sensitive") + "\n")
	b.WriteString("• " + tl("每人只能参与一次，重复发送会收到提醒", "One entry per user; repeats get a notice") + "\n")
	b.WriteString("• " + tl("开奖使用公平的加密随机数抽取", "Winners are drawn with fair crypto randomness") + "\n")
	b.WriteString("• " + tl("中奖者会收到私信通知，领奖时效 24 小时", "Winners get a DM; claim window is 24 h") + "\n")
	b.WriteString("\n💡 " + tl("示例: ", "Example: ") + plugin.Code(`lottery create 新年抽奖 抽奖 100 5 myprizes notify`))
	return b.String()
}

// cmdCreate: lottery create [标题] [关键词] [人数] [中奖数] [仓库/序号] [notify|at 时间]
func (p *LotteryPlugin) cmdCreate(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	args := ctx.Args[1:]
	if len(args) == 0 || (len(args) == 1 && strings.EqualFold(args[0], "list")) {
		return p.warehouseList(ctx)
	}
	if len(args) < 4 {
		return ctx.Edit("❌ " + tl("参数不足\n\n用法: ", "Not enough arguments\n\nUsage: ") +
			plugin.Code("lottery create <标题> <关键词> <人数> <中奖数> [仓库] [notify|at 时间]") +
			"\n\n💡 " + plugin.Code("lottery create list"))
	}
	title, keyword := args[0], args[1]
	maxUsers, err1 := strconv.Atoi(args[2])
	winners, err2 := strconv.Atoi(args[3])
	if err1 != nil || err2 != nil || maxUsers < 1 || winners < 1 || winners > maxUsers {
		return ctx.Edit("❌ " + tl("人数与中奖数必须是有效数字，且 1 ≤ 中奖数 ≤ 人数", "Max users and winners must be valid numbers with 1 ≤ winners ≤ max users"))
	}
	if strings.ContainsAny(title, "\n") || len([]rune(title)) > 64 || len([]rune(keyword)) > 32 {
		return ctx.Edit("❌ " + tl("标题最长 64 字，关键词最长 32 字", "Title max 64 chars, keyword max 32 chars"))
	}

	warehouse := "default"
	notify := false
	var at time.Time
	rest := args[4:]
	if len(rest) > 0 && !strings.EqualFold(rest[0], "notify") && !strings.EqualFold(rest[0], "at") {
		p.mu.Lock()
		names := p.warehouseNamesLocked()
		name, ok := warehouseByNameOrIndex(rest[0], names)
		var stock int
		if ok {
			stock = len(p.prizesInStockLocked(name))
		}
		p.mu.Unlock()
		if !ok {
			return ctx.Edit("❌ " + tl("奖品仓库不存在，可用仓库用 ", "No such warehouse; list them with ") + plugin.Code("lottery create list"))
		}
		if stock == 0 {
			return ctx.Edit("❌ " + tl("仓库中没有可用奖品: ", "Warehouse has no prizes in stock: ") + plugin.Code(name))
		}
		warehouse = name
		rest = rest[1:]
	}
	for i := 0; i < len(rest); i++ {
		switch strings.ToLower(rest[i]) {
		case "notify":
			notify = true
		case "at":
			if i+1 < len(rest) {
				t, err := parseAt(rest[i+1], time.Now())
				if err != nil {
					return ctx.Edit("❌ " + tl("定时时间格式错误: ", "Bad schedule time: ") + plugin.Escape(err.Error()))
				}
				at = t
				i++
			} else {
				return ctx.Edit("❌ " + tl("at 后面要跟时间，如 at 21:00", "at needs a time, e.g. at 21:00"))
			}
		}
	}

	chatID := ctx.Message.ChatID
	if chatID > 0 {
		return ctx.Edit("❌ " + tl("抽奖只能在群组中创建", "Lotteries can only be created in groups"))
	}

	p.mu.Lock()
	if l := p.activeLocked(chatID); l != nil {
		p.mu.Unlock()
		return ctx.Edit("❌ " + tl("当前群已有进行中的抽奖，请先开奖或删除", "This chat already has a running lottery; draw or delete it first"))
	}
	p.store.data.NextID++
	id := p.store.data.NextID
	l := &lottery{
		ID:        id,
		ChatID:    chatID,
		Title:     title,
		Keyword:   keyword,
		MaxUsers:  maxUsers,
		WinnerCnt: winners,
		Warehouse: warehouse,
		CreatorID: ctx.Message.UserID,
		CreatedAt: time.Now().Unix(),
		Status:    "active",
	}
	p.store.data.Lotteries = append(p.store.data.Lotteries, l)
	if !at.IsZero() {
		p.scheduleAutoLocked(id, at)
	}
	stock := len(p.prizesInStockLocked(warehouse))
	saveErr := p.saveLocked()
	p.mu.Unlock()
	if saveErr != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(saveErr.Error()))
	}

	var b strings.Builder
	b.WriteString("🎉 **" + tl("抽奖活动已创建", "Lottery created") + "**\n\n")
	b.WriteString("🏆 " + tl("活动", "Lottery") + "  " + plugin.Escape(title) + "\n")
	b.WriteString("🎁 " + tl("中奖名额", "Winners") + "  " + plugin.Bold(strconv.Itoa(winners)) + "\n")
	b.WriteString("👥 " + tl("参与上限", "Max entries") + "  " + plugin.Bold(strconv.Itoa(maxUsers)) + "\n")
	b.WriteString("🔑 " + tl("参与关键词", "Join keyword") + "  " + plugin.Code(keyword) + "\n")
	b.WriteString("📦 " + tl("奖品仓库", "Warehouse") + "  " + plugin.Code(warehouse) + " (" + strconv.Itoa(stock) + tl(" 种奖品", " prize kinds") + ")\n")
	if !at.IsZero() {
		b.WriteString("⏰ " + tl("定时开奖", "Scheduled draw") + "  " + plugin.Code(at.Format("2006-01-02 15:04")) + "\n")
	}
	b.WriteString("\n💡 " + tl("在群里发送关键词即可参与", "Send the keyword in the group to join"))

	msgID, err := p.host.Send(p.lifetime(), chatID, b.String(), 0)
	if err != nil {
		return ctx.Edit("❌ " + tl("发送抽奖公告失败: ", "Failed to post the announcement: ") + plugin.Escape(err.Error()))
	}
	p.mu.Lock()
	if l := p.findLocked(id); l != nil {
		l.MessageID = msgID
		p.saveLocked()
	}
	p.mu.Unlock()
	p.pinAnnouncement(p.lifetime(), chatID, msgID, notify)
	return ctx.Delete()
}

// warehouseList shows warehouses with stock (lottery create list).
func (p *LotteryPlugin) warehouseList(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	names := p.warehouseNamesLocked()
	wh := make([][2]int, len(names))
	for i, n := range names {
		items := p.prizesInStockLocked(n)
		stock := 0
		for _, it := range items {
			stock += it.Stock
		}
		wh[i] = [2]int{len(items), stock}
	}
	p.mu.Unlock()

	if len(names) == 0 {
		return ctx.Edit("📦 " + tl("暂无奖品仓库，先用 ", "No warehouses yet; create one with ") + plugin.Code("lottery prize create <名称>"))
	}
	var b strings.Builder
	b.WriteString("📦 **" + tl("奖品仓库", "Prize Warehouses") + "**\n\n")
	for i, n := range names {
		b.WriteString(fmt.Sprintf("%d. %s — %d %s, %s %d\n", i+1, plugin.Code(n), wh[i][0], tl("种奖品", "kinds"), tl("库存", "stock"), wh[i][1]))
	}
	b.WriteString("\n💡 " + plugin.Code("lottery create <标题> <关键词> <人数> <中奖数> <仓库/序号>"))
	return ctx.Edit(b.String())
}

// pinAnnouncement pins the lottery announcement like the source.
func (p *LotteryPlugin) pinAnnouncement(ctx context.Context, chatID int64, msgID int, notify bool) {
	cctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	peer, err := p.host.PeerResolver().ResolveFromChatID(cctx, chatID)
	if err != nil {
		p.log.Debug("lottery: resolve chat for pin failed", "error", err)
		return
	}
	req := &tg.MessagesUpdatePinnedMessageRequest{Peer: peer, ID: msgID}
	if !notify {
		req.SetSilent(true)
	}
	if _, err := p.host.API().MessagesUpdatePinnedMessage(cctx, req); err != nil {
		p.log.Debug("lottery: pin failed", "chat", chatID, "error", err)
	}
}

// cmdDraw draws the chat's active lottery (creator or admin).
func (p *LotteryPlugin) cmdDraw(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	chatID := ctx.Message.ChatID
	p.mu.Lock()
	l := p.activeLocked(chatID)
	var id int64
	var creator int64
	if l != nil {
		id, creator = l.ID, l.CreatorID
	}
	p.mu.Unlock()
	if id == 0 {
		return ctx.Edit("❌ " + tl("当前群没有进行中的抽奖活动", "No running lottery in this chat"))
	}
	if !p.canDraw(ctx, id, creator) {
		return ctx.Edit("❌ " + tl("只有抽奖创建者或群管理员可以开奖", "Only the lottery creator or a group admin can draw"))
	}
	_ = ctx.Edit("🔄 " + tl("开奖中…", "Drawing…"))
	// Draw synchronously so errors reach the user's command message.
	if err := p.performDraw(p.lifetime(), id, "manual"); err != nil {
		return ctx.Edit("❌ " + tl("开奖失败: ", "Draw failed: ") + plugin.Escape(err.Error()))
	}
	// The result card was posted to the chat; drop the command message.
	return ctx.Delete()
}

// canDraw checks creator or group-admin rights.
func (p *LotteryPlugin) canDraw(ctx *plugin.CommandContext, id, creator int64) bool {
	if ctx.Message.UserID == creator || ctx.IsSelf() {
		return true
	}
	return p.isAdmin(ctx.Context(), ctx.Message.ChatID, ctx.Message.UserID)
}

// cmdDelete removes the chat's active lottery (creator or admin).
func (p *LotteryPlugin) cmdDelete(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	chatID := ctx.Message.ChatID
	p.mu.Lock()
	l := p.activeLocked(chatID)
	if l == nil {
		p.mu.Unlock()
		return ctx.Edit("❌ " + tl("当前群没有进行中的抽奖活动", "No running lottery in this chat"))
	}
	id, creator, msgID := l.ID, l.CreatorID, l.MessageID
	p.mu.Unlock()
	if !p.canDraw(ctx, id, creator) {
		return ctx.Edit("❌ " + tl("只有抽奖创建者或群管理员可以删除活动", "Only the lottery creator or a group admin can delete"))
	}
	p.mu.Lock()
	out := p.store.data.Lotteries[:0]
	for _, x := range p.store.data.Lotteries {
		if x.ID != id {
			out = append(out, x)
		}
	}
	p.store.data.Lotteries = out
	saveErr := p.saveLocked()
	p.mu.Unlock()
	if saveErr != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(saveErr.Error()))
	}
	if msgID > 0 {
		go p.deleteMessages(p.lifetime(), chatID, msgID)
	}
	return ctx.Edit("✅ " + tl("已删除抽奖活动及相关数据", "Deleted the lottery and its data"))
}

// cmdStatus shows the chat's active lottery progress.
func (p *LotteryPlugin) cmdStatus(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	l := p.activeLocked(ctx.Message.ChatID)
	var l2 lottery
	if l != nil {
		l2 = *l
		l2.Participants = append([]participant(nil), l.Participants...)
	}
	p.mu.Unlock()
	if l2.ID == 0 {
		return ctx.Edit("📋 " + tl("当前群没有进行中的抽奖活动", "No running lottery in this chat"))
	}
	var b strings.Builder
	b.WriteString("📋 **" + tl("抽奖状态", "Lottery Status") + "**\n\n")
	b.WriteString("🏆 " + tl("活动", "Lottery") + "  " + plugin.Escape(l2.Title) + "\n")
	b.WriteString("🔑 " + tl("关键词", "Keyword") + "  " + plugin.Code(l2.Keyword) + "\n")
	b.WriteString("👥 " + tl("进度", "Progress") + "  " + fmt.Sprintf("%d/%d", len(l2.Participants), l2.MaxUsers) + "\n")
	b.WriteString("🎁 " + tl("中奖名额", "Winners") + "  " + strconv.Itoa(l2.WinnerCnt) + "\n")
	if l2.AutoDrawAt > 0 {
		b.WriteString("⏰ " + tl("定时开奖", "Scheduled draw") + "  " + plugin.Code(time.Unix(l2.AutoDrawAt, 0).Format("01-02 15:04")) + "\n")
	}
	b.WriteString("🕓 " + tl("创建时间", "Created") + "  " + plugin.Code(time.Unix(l2.CreatedAt, 0).Format("01-02 15:04")) + "\n")
	b.WriteString("🆔 " + tl("抽奖 ID", "Lottery ID") + "  " + plugin.Code(l2.ID))
	return ctx.Edit(b.String())
}

// cmdList lists participants of the chat's active lottery.
func (p *LotteryPlugin) cmdList(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	l := p.activeLocked(ctx.Message.ChatID)
	var l2 lottery
	if l != nil {
		l2 = *l
		l2.Participants = append([]participant(nil), l.Participants...)
	}
	p.mu.Unlock()
	if l2.ID == 0 {
		return ctx.Edit("❌ " + tl("当前群没有进行中的抽奖活动", "No running lottery in this chat"))
	}
	var b strings.Builder
	b.WriteString("👥 **" + tl("参与名单", "Participants") + "** — " + plugin.Escape(l2.Title) + "\n")
	b.WriteString("📊 " + fmt.Sprintf(tl("进度 %d/%d", "Progress %d/%d"), len(l2.Participants), l2.MaxUsers) + "\n\n")
	if len(l2.Participants) == 0 {
		b.WriteString(tl("暂无参与用户", "No participants yet"))
		return ctx.Edit(b.String())
	}
	for i, u := range l2.Participants {
		name := winnerName(winner{UserID: u.UserID, Username: u.Username, FirstName: u.FirstName, LastName: u.LastName})
		b.WriteString(fmt.Sprintf("%d. %s\n", i+1, plugin.Mention(plugin.Escape(name), u.UserID)))
	}
	out := b.String()
	if len([]rune(out)) > maxListLen {
		out = string([]rune(out)[:maxListLen]) + "\n…"
	}
	return ctx.Edit(out)
}

// cmdWinners shows winners and claim status; falls back to the latest
// completed lottery when the chat has no active one.
func (p *LotteryPlugin) cmdWinners(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	now := time.Now()
	p.expireClaimsLocked(now)
	l := p.activeLocked(ctx.Message.ChatID)
	if l == nil {
		// Latest completed lottery in this chat.
		var best *lottery
		for _, x := range p.store.data.Lotteries {
			if x.ChatID == ctx.Message.ChatID && x.Status == "completed" && len(x.Winners) > 0 {
				if best == nil || x.CreatedAt > best.CreatedAt {
					best = x
				}
			}
		}
		l = best
	}
	var l2 lottery
	if l != nil {
		l2 = *l
		l2.Winners = append([]winner(nil), l.Winners...)
	}
	saveErr := p.saveLocked()
	p.mu.Unlock()
	if saveErr != nil {
		p.log.Warn("lottery: save failed", "error", saveErr)
	}
	if l2.ID == 0 {
		return ctx.Edit("🏆 " + tl("本群暂无中奖记录", "No winners recorded in this chat yet"))
	}
	var b strings.Builder
	b.WriteString("🏆 **" + tl("中奖名单", "Winners") + "** — " + plugin.Escape(l2.Title) + "\n\n")
	for _, w := range l2.Winners {
		b.WriteString(statusIcon(w.Status) + " " + plugin.Mention(plugin.Escape(winnerName(w)), w.UserID) + " — " + plugin.Escape(w.Prize) + "\n")
	}
	b.WriteString("\n✅ " + tl("已发送", "delivered") + "  ⏳ " + tl("待领取", "pending") + "  ❌ " + tl("已过期", "expired"))
	return ctx.Edit(b.String())
}

// cmdClaim marks a winner's prize delivered, by ID or @username.
func (p *LotteryPlugin) cmdClaim(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	arg := ctx.GetArg(1)
	if arg == "" {
		return ctx.Edit("❌ " + tl("用法: ", "Usage: ") + plugin.Code("lottery claim <ID/@用户名>"))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Prefer the active lottery, else the chat's latest completed one.
	l := p.activeLocked(ctx.Message.ChatID)
	if l == nil {
		var best *lottery
		for _, x := range p.store.data.Lotteries {
			if x.ChatID == ctx.Message.ChatID && len(x.Winners) > 0 {
				if best == nil || x.CreatedAt > best.CreatedAt {
					best = x
				}
			}
		}
		l = best
	}
	if l == nil {
		return ctx.Edit("❌ " + tl("本群没有抽奖活动", "No lottery in this chat"))
	}
	target := strings.TrimPrefix(arg, "@")
	marked := false
	for i := range l.Winners {
		w := &l.Winners[i]
		match := false
		if strings.HasPrefix(arg, "@") {
			match = w.Username != "" && strings.EqualFold(w.Username, target)
		} else if id, err := strconv.ParseInt(arg, 10, 64); err == nil {
			match = w.UserID == id
		}
		if match {
			w.Status = "sent"
			marked = true
		}
	}
	if !marked {
		return ctx.Edit("❌ " + tl("未找到该中奖用户: ", "No such winner: ") + plugin.Code(arg))
	}
	if err := p.saveLocked(); err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + tl("已标记为已领奖", "Marked as delivered"))
}

// cmdPrize manages warehouses: create/add/list/clear.
func (p *LotteryPlugin) cmdPrize(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	cmd := strings.ToLower(ctx.GetArg(1))
	switch cmd {
	case "create":
		name := ctx.GetArg(2)
		if name == "" {
			return ctx.Edit("❌ " + tl("用法: ", "Usage: ") + plugin.Code("lottery prize create <仓库名>"))
		}
		p.mu.Lock()
		created := p.createWarehouseLocked(name)
		saveErr := p.saveLocked()
		p.mu.Unlock()
		if saveErr != nil {
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(saveErr.Error()))
		}
		if created {
			return ctx.Edit("✅ " + tl("仓库已创建: ", "Warehouse created: ") + plugin.Code(name))
		}
		return ctx.Edit("⚠️ " + tl("仓库已存在: ", "Warehouse already exists: ") + plugin.Code(name))
	case "add":
		// lottery prize add <仓库> <"奖品"> <数量> — the prize text may be
		// quoted to contain spaces, like the source.
		fields, err := parsePrizeAdd(ctx.RawArgs)
		if err != nil {
			return ctx.Edit("❌ " + tl("参数格式错误\n\n用法: ", "Bad arguments\n\nUsage: ") + plugin.Code(`lottery prize add <仓库> <"奖品内容"> <数量>`))
		}
		warehouse, text, stockStr := fields[0], fields[1], fields[2]
		stock, err := strconv.Atoi(stockStr)
		if err != nil || stock < 1 || stock > 100000 {
			return ctx.Edit("❌ " + tl("数量必须是 1-100000 的数字", "Stock must be a number 1-100000"))
		}
		p.mu.Lock()
		if _, ok := p.store.data.Warehouses[warehouse]; !ok {
			p.mu.Unlock()
			return ctx.Edit("❌ " + tl("仓库不存在，先创建: ", "No such warehouse; create it first: ") + plugin.Code(warehouse))
		}
		p.addPrizeLocked(warehouse, text, stock)
		total := 0
		for _, it := range p.store.data.Warehouses[warehouse] {
			total += it.Stock
		}
		saveErr := p.saveLocked()
		p.mu.Unlock()
		if saveErr != nil {
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(saveErr.Error()))
		}
		return ctx.Edit("✅ " + tl("奖品已添加", "Prize added") + "\n\n" +
			"📦 " + tl("仓库", "Warehouse") + "  " + plugin.Code(warehouse) + "\n" +
			"🎁 " + tl("奖品", "Prize") + "  " + plugin.Escape(text) + "\n" +
			"🔢 " + tl("数量", "Stock") + "  " + plugin.Bold(strconv.Itoa(stock)) + "\n" +
			"📊 " + tl("总库存", "Total") + "  " + plugin.Bold(strconv.Itoa(total)))
	case "list":
		name := ctx.GetArg(2)
		p.mu.Lock()
		defer p.mu.Unlock()
		if name == "" {
			names := p.warehouseNamesLocked()
			if len(names) == 0 {
				return ctx.Edit("📦 " + tl("暂无奖品仓库", "No prize warehouses"))
			}
			var b strings.Builder
			b.WriteString("📦 **" + tl("奖品仓库", "Prize Warehouses") + "**\n\n")
			for i, n := range names {
				items := p.prizesInStockLocked(n)
				stock := 0
				for _, it := range items {
					stock += it.Stock
				}
				b.WriteString(fmt.Sprintf("%d. %s — %s %d\n", i+1, plugin.Code(n), tl("库存", "stock"), stock))
			}
			return ctx.Edit(b.String())
		}
		items := p.prizesInStockLocked(name)
		if len(items) == 0 {
			return ctx.Edit("📦 " + tl("仓库暂无可用奖品: ", "Warehouse has no prizes in stock: ") + plugin.Code(name))
		}
		var b strings.Builder
		b.WriteString("📦 **" + tl("仓库", "Warehouse") + "  " + plugin.Escape(name) + "**\n\n")
		for i, it := range items {
			b.WriteString(fmt.Sprintf("%d. %s — %s %d\n", i+1, plugin.Escape(it.Text), tl("库存", "stock"), it.Stock))
		}
		return ctx.Edit(b.String())
	case "clear":
		target := ctx.GetArg(2)
		if target == "" {
			return ctx.Edit("❌ " + tl("用法: ", "Usage: ") + plugin.Code("lottery prize clear <仓库|all>"))
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if strings.EqualFold(target, "all") {
			n := 0
			for _, items := range p.store.data.Warehouses {
				n += len(items)
			}
			p.store.data.Warehouses = map[string][]prizeItem{}
			if err := p.saveLocked(); err != nil {
				return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
			}
			return ctx.Edit("✅ " + fmt.Sprintf(tl("已清空所有仓库（%d 条奖品）", "Cleared all warehouses (%d prizes)"), n))
		}
		if _, ok := p.store.data.Warehouses[target]; !ok {
			return ctx.Edit("❌ " + tl("仓库不存在: ", "No such warehouse: ") + plugin.Code(target))
		}
		n := p.clearWarehouseLocked(target)
		if err := p.saveLocked(); err != nil {
			return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
		}
		return ctx.Edit("✅ " + fmt.Sprintf(tl("已清空仓库 %s（%d 条奖品）", "Cleared %s (%d prizes)"), plugin.Code(target), n))
	}
	return ctx.Edit("❌ " + tl("未知子命令: ", "Unknown subcommand: ") + plugin.Code(cmd) + "\n\n💡 " + plugin.Code("lottery help"))
}

// parsePrizeAdd splits the raw args after "prize add" into warehouse,
// prize text (possibly quoted) and stock count.
func parsePrizeAdd(raw string) ([3]string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "prize")
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "add")
	s = strings.TrimSpace(s)
	if s == "" {
		return [3]string{}, fmt.Errorf("empty")
	}
	var warehouse, text, stock string
	rest := s
	if strings.HasPrefix(s, "\"") {
		return [3]string{}, fmt.Errorf("warehouse cannot be quoted")
	}
	i := strings.IndexAny(rest, " \t")
	if i < 0 {
		return [3]string{}, fmt.Errorf("missing prize text")
	}
	warehouse, rest = rest[:i], strings.TrimSpace(rest[i:])
	if strings.HasPrefix(rest, "\"") {
		j := strings.Index(rest[1:], "\"")
		if j < 0 {
			return [3]string{}, fmt.Errorf("unbalanced quotes")
		}
		text, rest = rest[1:1+j], strings.TrimSpace(rest[1+j+1:])
	} else {
		j := strings.IndexAny(rest, " \t")
		if j < 0 {
			return [3]string{}, fmt.Errorf("missing stock")
		}
		text, rest = rest[:j], strings.TrimSpace(rest[j:])
	}
	k := strings.IndexAny(rest, " \t")
	if k >= 0 {
		stock = rest[:k]
	} else {
		stock = rest
	}
	if warehouse == "" || text == "" || stock == "" {
		return [3]string{}, fmt.Errorf("missing fields")
	}
	return [3]string{warehouse, text, stock}, nil
}
