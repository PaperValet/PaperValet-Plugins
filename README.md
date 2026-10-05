<div align="center">

# PaperValet-Plugins

External plugins for [PaperValet](https://github.com/PaperValet/PaperValet) · [PaperValet](https://github.com/PaperValet/PaperValet) 的外部插件

</div>

## 安装 / Install

```
.apt s            浏览 / browse
.apt i weather    安装 / install
.apt i -all       全部安装 / install all
.apt rm weather   卸载 / remove
```

## 插件 / Plugins

| 插件 Plugin | 命令 Command | 作用 | What it does |
|---|---|---|---|
| acron | `acron` | Cron 定时发送/转发/复制/删除/置顶/执行命令 | Cron-scheduled send/forward/copy/delete/pin/run-command |
| ai | `ai` | AI 对话，可配置模型和提示词 | Chat with an AI, configurable model and prompt |
| annualreport | `annualreport` | 生成群年度/时段报告：活跃、 Top 发言、时段分布 | Chat stats report: activity, top talkers, hour bins |
| atadmins | `atadmins` | 一键艾特全部管理员 | Mention every group admin |
| autochangename | `autochangename` | 自动轮换账号名字/简介 | Auto-rotate the account name or bio |
| autodel | `autodel` | 定时自动删自己的消息，含命令输出清理 | Auto-delete your messages on a timer, incl. command outputs |
| ban | `ban` · `unban` · `kick` · `mute` · `unmute` · `sb` · `unsb` · `refresh` | 封禁、踢出、禁言与跨群批量封禁 | Ban, kick, mute and cross-group batch bans |
| bgp | `bgp` | 查询 IP/ASN/前缀的 BGP 路由信息 | BGP routing info for an IP/ASN/prefix |
| bizhi | `bizhi` | 随机高清壁纸 | Random wallpaper |
| bs | `bs` | 回复消息一键保送到多个目标，顺序或群发 | Forward replied messages to multiple targets, sequence or broadcast |
| checkin | `checkin` | 每日自动签到：定时向目标机器人发送签到命令并点击签到按钮 | Daily auto check-in: sends sign-in commands to target bots on schedule and clicks buttons |
| clean | `clean` | 批量清理消息/成员消息/贴纸状态，支持确认 | Bulk-clean messages, a member's messages or sticker state, with confirm |
| cosplay | `cosplay` · `cos` | 随机 Cosplay 图片 | Random cosplay photos |
| crazy4 | `crazy4` | 随机疯狂星期四文案，一轮内不重复 | Random Crazy Thursday copypasta, no repeats per round |
| diss | `diss` | 儒雅随和版祖安语录 | Random polite roast |
| duckduckgo | `duckduckgo` · `ddg` | DuckDuckGo 网页搜索 | Web search |
| gt | `gt` | 谷歌翻译 | Google Translate |
| his | `his` | 查看某人在群里的发言记录 | List someone's recent messages in a group |
| hitokoto | `hitokoto` | 随机一言 | Random quote |
| ip | `ip` | 查询 IP 或域名的归属地 | Look up where an IP or domain lives |
| keyword | `keyword` | 关键词自动回复，支持正则与冷却 | Auto-reply to keywords, regex and cooldowns |
| listusernames | `listusernames` | 列出自己的公开群组和频道 | List your public groups and channels |
| lottery | `lottery` | 群抽奖：报名、定时开奖 | Group lottery: join by keyword, scheduled draw |
| luxiaoxunbs | `luxiaoxunbs` | 鲁小迅整点报时，每小时发贴纸时钟并撤回上一条 | Lu Xiaoxun hourly sticker clock, previous one auto-deleted |
| music | `music` | 通过音乐机器人搜歌发歌，支持多平台和先搜后选 | Songs via music bots, multi-platform, search then pick |
| news | `news` | 每日新闻、历史上的今天、成语和诗词 | Daily Chinese news digest |
| pangu | `pangu` | 中英文之间加空格（盘古之白） | Space CJK and Latin text (pangu) |
| paolu | `paolu` | 一键跑路：禁言全员并清空群消息 | Mute everyone and wipe the group history |
| parsehub | `parsehub` | 解析链接并转发结果 | Parse links and forward the results |
| pmcaptcha | `pmc` | 私信验证码门禁 | PM captcha gate |
| portball | `portball` | 回复消息限时禁言，到期自动解除 | Mute someone by reply for a set time |
| postsearch | `postsearch` | 搜索频道/群组帖子 | Search channel and group posts |
| premium | `premium` | 统计群里的 Premium 会员 | Count Premium members in a group |
| quote | `quote` | 回复消息生成引用图，本地纯 Go 渲染 | Turn replied messages into a quote image, rendered locally in pure Go |
| rev | `rev` | 反转文字，翻转或反色媒体 | Reverse text, flip or invert media |
| save | `save` | 保存和转发消息，绕过禁止转发 | Save messages, even from no-forward chats |
| sendat | `sendat` | 定时发送消息，重启不丢 | Scheduled messages that survive restarts |
| setu | `setu` | 通过 FinelyGirls 机器人获取二次元图片，带剧透与签到 | Anime images via the FinelyGirls bot, spoiler-wrapped, with daily check-in |
| shift | `shift` | 批量转移消息到指定对话 | Move messages to another chat |
| speedtest | `speedtest` · `st` | Ookla 网速测试 | Speedtest by Ookla |
| sticker | `sticker` | 贴纸包管理、图转贴纸、贴纸转图 | Sticker pack management, photo→sticker, sticker→photo |
| subinfo | `subinfo` · `cha` | 查询机场订阅流量与节点信息 | Query proxy subscription traffic and node info |
| teletype | `teletype` | 打字机效果：逐字打出文本（带光标），可自动加到自己消息上 | Typewriter effect: retype text char by char (with cursor), auto mode for own messages |
| textmode | `textmode` | 自动给自己发的消息加格式（粗体/下划线/遮罩等） | Auto-format your own messages (bold/underline/spoiler…) |
| trace | `trace` | 自动给指定用户或关键字的消息贴表情回应 | Auto-react to messages from chosen users or with keywords |
| weather | `weather` | 天气查询 | Weather |
| whois | `whois` | 查看用户/群组详细信息 | Detailed user or chat info |
| wordcloud | `wordcloud` | 从群聊天记录生成词云，可定时推送 | Word cloud from chat history, schedulable |
| xmsl | `xmsl` | AI 生成一句羡慕死了回复，支持图片 | AI-generated envious one-liners, image aware |
| yvlu | `yvlu` | 回复消息生成引用贴纸/图片，可存贴纸包 | Turn replied messages into quote stickers/images, saveable to a pack |
| zpr | `zpr` | Lolicon 随机图片，多反代自动切换 | Random images via Lolicon with mirror auto-switching |

每个命令都支持 `help`，比如 `.weather help`。只有 duckduckgo 和 speedtest 带简写，其他短命令用 `.alias` 自己加。
Every command takes `help`, e.g. `.weather help`. Only duckduckgo and speedtest ship a short alias; add your own with `.alias`.

插件的选项不走命令，都在配套机器人的 /menu 按钮面板里：weather 默认城市、gt 默认语言、duckduckgo 条数、hitokoto 默认类型、bizhi 默认分类、atadmins 召唤消息、save 保存目标、sendat 时区、speedtest 服务器和结果类型；sendat 和 speedtest 还有任务列表、附近服务器页面。
Plugin options are not commands: they live in the companion bot's /menu panel — weather's default city, gt's default language, duckduckgo's result count, hitokoto's default type, bizhi's default category, atadmins' message, save's target, sendat's timezone, speedtest's server and result type; sendat and speedtest also add task-list and nearby-server pages.

网络延迟测试已经并入内置的 `.ping`。Latency tests moved into the built-in `.ping`.

## 开发 / Development

One directory per plugin under `plugins-external/`, with its own `go.mod`. See the [Plugin SDK](https://github.com/PaperValet/PaperValet/blob/master/docs/plugin-sdk.md) / [插件 SDK](https://github.com/PaperValet/PaperValet/blob/master/docs/plugin-sdk_zh.md).

```bash
cd plugins-external/weather
go work init . ../../../PaperValet
go test ./...
go build -trimpath -buildmode=plugin -o weather.so .
```

Go version must match the PaperValet release exactly (currently 1.25.14), and the build must go through the workspace, not the `replace` directive.

On every push to `main`, CI builds each plugin against the latest PaperValet, then republishes the [`latest`](https://github.com/PaperValet/PaperValet-Plugins/releases/tag/latest) release with all `.so` files and the `plugins.json` index generated by `scripts/gen-registry.py`.

## License

MIT
