package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// handleMe is the port of the source's account-level annual report: dialog
// census, blocked count, premium status, run days and a hitokoto quote.
func (p *AnnualReportPlugin) handleMe(ctx *plugin.CommandContext) error {
	_ = ctx.Edit("🔄 " + ctx.Tlocal("加载中请稍候。。。", "Loading, please wait…"))
	api := ctx.API
	if api == nil {
		return ctx.Edit("❌ " + ctx.Tlocal("无法获取客户端", "No API client available"))
	}
	c, cancel := context.WithTimeout(p.lifetime(), 5*time.Minute)
	defer cancel()
	tl := ctx.Tlocal

	year := reportYear(time.Now())
	stats, err := dialogCensus(c, api)
	if err != nil && ctx.Logger != nil {
		ctx.Logger.Warn("annualreport: dialogs failed", "error", err)
	}
	blocked, berr := blockedCount(c, api)
	if berr != nil && ctx.Logger != nil {
		ctx.Logger.Warn("annualreport: blocked failed", "error", berr)
	}
	user := selfName(c, api)
	p.bumpCount()

	name := user.display(tl)
	var b strings.Builder
	b.WriteString("🎉 " + plugin.Bold(name) + " · " + tl("年度报告", "Year in review") + " " + plugin.Code(strconv.Itoa(year)) + "\n\n")

	b.WriteString("📅 **" + tl("陪伴时光", "Time together") + "**\n")
	b.WriteString("> " + fmt.Sprintf(tl("PaperValet 已陪伴你 %d 天", "PaperValet has been with you for %d days"), runDays(p.dataDir())) + "\n")
	b.WriteString("> " + fmt.Sprintf(tl("已生成 %d 份报告", "%d reports generated"), loadState(filepath.Join(p.dataDir(), stateFile)).ReportCount) + "\n\n")

	b.WriteString("👥 **" + tl("社交网络", "Social graph") + "**\n")
	if err != nil {
		b.WriteString("> " + tl("会话统计获取失败，请稍后重试", "Dialog census failed, try again later") + "\n\n")
	} else {
		b.WriteString("> " + fmt.Sprintf(tl("%d 个频道 · %d 个群组", "%d channels · %d groups"), stats.Channels, stats.Groups) + "\n")
		b.WriteString("> " + fmt.Sprintf(tl("%d 个私聊 · %d 个机器人", "%d private chats · %d bots"), stats.Private, stats.Bots) + "\n")
		b.WriteString("> " + tl("愿你的生活每天都像庆典一样开心", "May every day feel like a celebration") + "\n\n")
	}

	b.WriteString("🛡️ **" + tl("安全守护", "Safety") + "**\n")
	if berr != nil {
		b.WriteString("> " + tl("黑名单数量获取失败，请稍后重试", "Blocklist count failed, try again later") + "\n")
	} else {
		b.WriteString("> " + fmt.Sprintf(tl("黑名单 %d 人", "%d blocked"), blocked) + "\n")
		if blocked < 20 {
			b.WriteString("> " + tl("你的账户真的很干净", "Your account is really clean") + "\n")
		} else {
			b.WriteString("> " + tl("愿明年的 spam 少一些", "May next year bring less spam") + "\n")
		}
	}
	if user.Premium {
		b.WriteString("\n⭐ **" + tl("会员特权", "Premium") + "**\n")
		b.WriteString("> " + tl("你已成为 TG 大会员用户，愿新一年继续享受专属特权", "You are a Telegram Premium member — enjoy the perks another year") + "\n")
	}

	b.WriteString("\n💫 **" + tl("年度寄语", "Words of the year") + "**\n")
	b.WriteString("> " + p.hitokoto(ctx) + "\n\n")
	b.WriteString(plugin.Code("#" + strconv.Itoa(year) + tl("年度报告", "AnnualReport")))
	return ctx.Edit(b.String())
}

// reportYear picks the year under review: in January it is still last year
// (as in the source).
func reportYear(now time.Time) int {
	y := now.Year()
	if now.Month() == time.January {
		y--
	}
	return y
}

// userInfo is what we need from users.getUsers for the report header.
type userInfo struct {
	Username string
	First    string
	Last     string
	Premium  bool
	OK       bool
}

func (u userInfo) display(tl func(string, string) string) string {
	if !u.OK {
		return tl("未知用户", "unknown user")
	}
	switch {
	case u.Username != "":
		return plugin.Escape("@" + u.Username)
	case u.First != "" && u.Last != "":
		return plugin.Escape(u.First + " " + u.Last)
	case u.First != "":
		return plugin.Escape(u.First)
	default:
		return tl("未知用户", "unknown user")
	}
}

func selfName(ctx context.Context, api *tg.Client) userInfo {
	res, err := api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	if err != nil || len(res) == 0 {
		return userInfo{}
	}
	u, ok := res[0].(*tg.User)
	if !ok {
		return userInfo{}
	}
	return userInfo{Username: u.Username, First: u.FirstName, Last: u.LastName, Premium: u.Premium, OK: true}
}

// census is the dialog breakdown the source computed.
type census struct {
	Private  int
	Groups   int
	Bots     int
	Channels int
}

// dialogCensus walks messages.getDialogs over the main list and archived
// folder 1 and counts dialog kinds.
func dialogCensus(ctx context.Context, api *tg.Client) (census, error) {
	var out census
	seen := map[int64]bool{}
	for _, folder := range []int{0, 1} {
		offsetDate, offsetID := 0, 0
		var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
		for {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			req := &tg.MessagesGetDialogsRequest{
				OffsetDate: offsetDate,
				OffsetID:   offsetID,
				OffsetPeer: offsetPeer,
				Limit:      100,
			}
			if folder != 0 {
				req.SetFolderID(folder)
			}
			res, err := api.MessagesGetDialogs(ctx, req)
			if err != nil {
				return out, err
			}
			var page dialogPage
			switch v := res.(type) {
			case *tg.MessagesDialogs:
				page = dialogPage{v.Dialogs, v.Chats, v.Users, v.Messages}
			case *tg.MessagesDialogsSlice:
				page = dialogPage{v.Dialogs, v.Chats, v.Users, v.Messages}
			default:
				return out, nil
			}
			if len(page.dialogs) == 0 {
				break
			}
			users := map[int64]*tg.User{}
			for _, u := range page.users {
				if v, ok := u.(*tg.User); ok {
					users[v.ID] = v
				}
			}
			chats := map[int64]tg.ChatClass{}
			for _, c := range page.chats {
				switch v := c.(type) {
				case *tg.Channel:
					chats[v.ID] = v
				case *tg.Chat:
					chats[v.ID] = v
				}
			}
			for _, d := range page.dialogs {
				dl, ok := d.(*tg.Dialog)
				if !ok {
					continue
				}
				var id int64
				isUser, isBot := false, false
				isGroup, isChannel := false, false
				switch pr := dl.Peer.(type) {
				case *tg.PeerUser:
					id = pr.UserID
					isUser = true
					if u, ok := users[id]; ok {
						isBot = u.Bot
					}
				case *tg.PeerChat:
					id = -pr.ChatID
					isGroup = true
				case *tg.PeerChannel:
					id = plugin.ChannelChatID(pr.ChannelID)
					if ch, ok := chats[pr.ChannelID].(*tg.Channel); ok {
						isChannel = ch.Broadcast
						isGroup = ch.Megagroup
					} else {
						isChannel = true
					}
				default:
					continue
				}
				seen[id] = true
				switch {
				case isUser && isBot:
					out.Bots++
				case isUser:
					out.Private++
				case isGroup:
					out.Groups++
				case isChannel:
					out.Channels++
				}
			}
			if len(page.dialogs) < 100 {
				break
			}
			last := page.dialogs[len(page.dialogs)-1]
			if dl, ok := last.(*tg.Dialog); ok {
				offsetID = dl.TopMessage
				switch pr := dl.Peer.(type) {
				case *tg.PeerUser:
					offsetPeer = &tg.InputPeerUser{UserID: pr.UserID}
				case *tg.PeerChat:
					offsetPeer = &tg.InputPeerChat{ChatID: pr.ChatID}
				case *tg.PeerChannel:
					offsetPeer = &tg.InputPeerChannel{ChannelID: pr.ChannelID}
				}
			}
			offsetDate = 0
			if len(page.msgs) > 0 {
				if m, ok := page.msgs[len(page.msgs)-1].(*tg.Message); ok && m.Date > 0 {
					offsetDate = m.Date
				}
			}
		}
	}
	return out, nil
}

type dialogPage struct {
	dialogs []tg.DialogClass
	chats   []tg.ChatClass
	users   []tg.UserClass
	msgs    []tg.MessageClass
}

// blockedCount returns the size of the blocklist (contacts.getBlocked).
func blockedCount(ctx context.Context, api *tg.Client) (int, error) {
	res, err := api.ContactsGetBlocked(ctx, &tg.ContactsGetBlockedRequest{Offset: 0, Limit: 1})
	if err != nil {
		return 0, err
	}
	switch v := res.(type) {
	case *tg.ContactsBlockedSlice:
		return v.Count, nil
	case *tg.ContactsBlocked:
		return len(v.Users), nil
	}
	return 0, nil
}

// runDays counts days since first run (source used LICENSE mtime; the
// plugin's own stats.json startTime is the portable equivalent).
func runDays(dir string) int {
	s := loadState(filepath.Join(dir, stateFile))
	return int(time.Since(time.Unix(s.StartTime, 0)).Hours() / 24)
}

// hitokoto fetches the quote of the year (source behavior, network best
// effort with the source's fallback line).
func (p *AnnualReportPlugin) hitokoto(ctx *plugin.CommandContext) string {
	hctx, cancel := context.WithTimeout(ctx.Context(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, hitokotoURL+"?charset=utf-8&encode=json", nil)
	if err == nil {
		req.Header.Set("User-Agent", "PaperValet-AnnualReport/1.0")
		if resp, err := p.http.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var data struct {
					Hitokoto string  `json:"hitokoto"`
					From     string  `json:"from"`
					FromWho  *string `json:"from_who"`
				}
				if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data) == nil && data.Hitokoto != "" {
					q := "\"" + plugin.Escape(data.Hitokoto) + "\" —— "
					if data.FromWho != nil && *data.FromWho != "" && *data.FromWho != "null" {
						q += plugin.Escape(*data.FromWho)
					}
					if data.From != "" {
						q += "「" + plugin.Escape(data.From) + "」"
					}
					return q
				}
			}
		}
	}
	return ctx.Tlocal(
		"\"用代码表达言语的魅力，用代码书写山河的壮丽。\" —— 一言「一言开发者中心」",
		"\"Code speaks where words fail, and writes the grandeur of rivers and mountains.\" — Hitokoto")
}
