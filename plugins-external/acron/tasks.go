package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/robfig/cron/v3"
)

// Task types.
const (
	typeSend    = "send"
	typeCmd     = "cmd"
	typeCopy    = "copy"
	typeForward = "forward"
	typeDel     = "del"
	typeDelRe   = "del_re"
	typePin     = "pin"
	typeUnpin   = "unpin"
)

var taskTypes = []string{typeSend, typeCmd, typeCopy, typeForward, typeDel, typeDelRe, typePin, typeUnpin}

// taskTypeLabel maps a task type to bilingual labels.
func taskTypeLabel(tl func(string, string) string, tp string) string {
	switch tp {
	case typeSend:
		return tl("定时发送", "send")
	case typeCmd:
		return tl("定时命令", "cmd")
	case typeCopy:
		return tl("定时复制", "copy")
	case typeForward:
		return tl("定时转发", "forward")
	case typeDel:
		return tl("定时删除", "delete")
	case typeDelRe:
		return tl("正则删除", "regex delete")
	case typePin:
		return tl("定时置顶", "pin")
	case typeUnpin:
		return tl("取消置顶", "unpin")
	}
	return tp
}

// Task is one persisted cron task (data/acron/tasks.json).
// Field names follow the TeleBox acron schema where practical.
type Task struct {
	ID      int      `json:"id"`
	Type    string   `json:"type"`
	Cron    string   `json:"cron"`
	Chat    string   `json:"chat"`              // user-typed chat arg (id or @name), kept for display/copy
	ChatID  int64    `json:"chat_id,omitempty"` // resolved chat id (users +, groups -, channels -100…)
	Peer    *peerRef `json:"peer,omitempty"`    // resolved InputPeer, no re-resolve needed on run
	Display string   `json:"display,omitempty"`
	Remark  string   `json:"remark,omitempty"`

	// send
	Message  string   `json:"message,omitempty"`  // text to send (send) or command line (cmd)
	Entities []string `json:"entities,omitempty"` // base64 TL entities, send only
	ReplyTo  int      `json:"reply_to,omitempty"` // topic top msg id or reply msg id

	// copy / forward
	FromPeer *peerRef `json:"from_peer,omitempty"` // source chat
	FromChat string   `json:"from_chat,omitempty"` // source chat display/id, informational
	FromMsg  int      `json:"from_msg,omitempty"`  // source message id

	// del / pin / unpin
	MsgID int `json:"msg_id,omitempty"`
	// pin
	Notify    bool `json:"notify,omitempty"`
	PmOneSide bool `json:"pm_one_side,omitempty"`

	// del_re
	Limit int    `json:"limit,omitempty"`
	Regex string `json:"regex,omitempty"`

	CreatedAt  int64  `json:"created_at"`
	LastRunAt  int64  `json:"last_run_at,omitempty"`
	LastResult string `json:"last_result,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	Disabled   bool   `json:"disabled,omitempty"`
}

// cronParser parses 6-field expressions (sec min hour dom mon dow).
var cronParser = cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// parseCronExpr validates a 6-field cron expression and returns its schedule.
func parseCronExpr(expr string) (cron.Schedule, error) {
	return cronParser.Parse(expr)
}

// parseCronFromArgs takes the first 6 whitespace-separated args as the cron
// expression; the remainder are the command's own arguments.
func parseCronFromArgs(args []string) (string, []string, error) {
	if len(args) < 6 {
		return "", nil, fmt.Errorf("need 6 fields: sec min hour dom mon dow")
	}
	expr := strings.Join(args[:6], " ")
	if _, err := parseCronExpr(expr); err != nil {
		return "", nil, err
	}
	return expr, args[6:], nil
}

// parseChatArg splits "chatArg|topicOrReply" into the chat part and an
// optional reply/topic id. Also accepts the full-width ｜ separator.
func parseChatArg(s string) (chat string, replyTo int, err error) {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '|' || r == '｜'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return "", 0, fmt.Errorf("empty chat")
	}
	chat = out[0]
	if len(out) > 1 {
		n, e := strconv.Atoi(out[1])
		if e != nil || n <= 0 {
			return "", 0, fmt.Errorf("bad reply/topic id %q", out[1])
		}
		replyTo = n
	}
	return chat, replyTo, nil
}

// parseChatID parses a numeric chat id (marked -100…/-… channel/group form
// or plain positive user id). ok=false for @usernames and junk.
func parseChatID(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// tryParseRegex parses "/pattern/flags" or a bare Go regexp.
func tryParseRegex(input string) (*regexp.Regexp, error) {
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(trimmed, "/") {
		if last := strings.LastIndex(trimmed, "/"); last > 0 {
			pattern, flags := trimmed[1:last], trimmed[last+1:]
			var opts []string
			for _, f := range flags {
				switch f {
				case 'i':
					opts = append(opts, "i")
				case 'm':
					opts = append(opts, "m")
				case 's':
					opts = append(opts, "s")
				case 'U':
					opts = append(opts, "U")
				default:
					return nil, fmt.Errorf("unsupported flag %q (Go supports i m s U)", f)
				}
			}
			return regexp.Compile("(?" + strings.Join(opts, "") + ":" + pattern + ")")
		}
	}
	return regexp.Compile(trimmed)
}

// parseBoolArg accepts 1/true/yes/y (case-insensitive) as true.
func parseBoolArg(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "y":
		return true
	}
	return false
}

// buildCopyCommand rebuilds the command line that recreates this task.
func buildCopyCommand(prefix string, t Task) string {
	c := t.Cron
	chat := t.Chat
	if chat == "" && t.ChatID != 0 {
		chat = strconv.FormatInt(t.ChatID, 10)
	}
	remark := ""
	if t.Remark != "" {
		remark = " " + t.Remark
	}
	switch t.Type {
	case typeSend, typeCmd:
		reply := ""
		if t.ReplyTo != 0 {
			reply = "|" + strconv.Itoa(t.ReplyTo)
		}
		head := fmt.Sprintf("%sacron %s %s %s%s%s", prefix, t.Type, c, chat, reply, remark)
		if t.Type == typeCmd {
			return head + "\n" + t.Message
		}
		return head
	case typeCopy, typeForward:
		reply := ""
		if t.ReplyTo != 0 {
			reply = "|" + strconv.Itoa(t.ReplyTo)
		}
		// source chat/message come from the reply at creation time
		return fmt.Sprintf("%sacron %s %s %s%s%s", prefix, t.Type, c, chat, reply, remark)
	case typeDel:
		return fmt.Sprintf("%sacron del %s %s %d%s", prefix, c, chat, t.MsgID, remark)
	case typeDelRe:
		return fmt.Sprintf("%sacron del_re %s %s %d %s%s", prefix, c, chat, t.Limit, t.Regex, remark)
	case typePin:
		n, pm := 0, 0
		if t.Notify {
			n = 1
		}
		if t.PmOneSide {
			pm = 1
		}
		return fmt.Sprintf("%sacron pin %s %s %d %d %d%s", prefix, c, chat, t.MsgID, n, pm, remark)
	case typeUnpin:
		return fmt.Sprintf("%sacron unpin %s %s %d%s", prefix, c, chat, t.MsgID, remark)
	}
	return prefix + "acron"
}
