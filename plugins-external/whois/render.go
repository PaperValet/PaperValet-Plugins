package main

import (
	"errors"
	"fmt"

	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// helpText renders the whois usage card.
func helpText(tl func(string, string) string) string {
	return "🔍 **" + tl("信息查询", "Whois") + "**\n\n> " +
		plugin.Code("whois") + " " + tl("回复某人时查他，无参数查自己", "reply to someone to look them up; no argument shows yourself") + "\n> " +
		plugin.Code("whois @用户名") + " " + tl("按用户名查询", "look up by username") + "\n> " +
		plugin.Code(tl("whois <ID>", "whois <ID>")) + " " + tl("正数为用户，-100… 为频道/超级群，-… 为普通群", "positive for users, -100… for channels/supergroups, -… for basic groups") + "\n\n" +
		tl("用户卡片：ID、用户名、手机、注册估算、DC、上线状态、简介、共同群、个人频道、入群时间；群卡片：成员/管理员/在线数、链接、关联讨论组",
			"User card: ID, usernames, phone, estimated registration, DC, last seen, bio, common groups, personal channel, join date; chat card: member/admin/online counts, links, linked discussion group")
}

// fail renders a lookup error bilingually.
func fail(tl func(string, string) string, err error) string {
	out := "❌ **" + tl("查询失败", "Lookup failed") + "**\n\n"
	if d, ok := tgerr.AsFloodWait(err); ok {
		return out + fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "Flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	var e *tgerr.Error
	if errors.As(err, &e) {
		switch e.Type {
		case "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID":
			return out + tl("这个用户名不存在", "That username does not exist")
		case "CHANNEL_PRIVATE", "CHAT_FORBIDDEN", "CHANNEL_INVALID", "CHAT_ID_INVALID":
			return out + tl("无法访问该对象（可能是私有群组）", "Cannot access that target (it may be a private chat)")
		case "PEER_ID_INVALID", "INPUT_USER_DEACTIVATED", "USER_ID_INVALID":
			return out + tl("找不到这个对象", "Cannot find that target")
		}
		return out + plugin.Code(e.Type)
	}
	return out + plugin.Escape(err.Error())
}
