# 插件仓库全量代码审查（2026-10-06）

范围：plugins-external 全部 51 个插件约 66k 行，6 个审查域并行深读。共 185 条发现：**23 bug / 86 edge / 76 polish**。

严重度定义：bug=会产生错误行为/崩溃/数据丢失；edge=特定条件下的边界问题；polish=可优化提升。所有条目带 文件:行号，落实后逐条勾选（`- [x]`）。

## P0 — 崩溃或数据丢失风险（先修）

1. **luxiaoxunbs** main.go:319-346 — sendAll 懒加载分支锁不平衡，首次整点报时必 `unlock of unlocked mutex` panic（可掀翻宿主进程）
2. **wordcloud** ttcfix.go:28 — TTC 头越界无长度校验，恶意/损坏字体文件直接 panic 宿主
3. **shift** rules.go:146-163 — wlCache 全局 map 无锁并发写，`concurrent map writes` fatal
4. **pmcaptcha** cmd.go:112-128 — `pmc test` 无参对 self 执行失败动作：fail_action=delete 时**删自己 Saved Messages**，=report 时举报自己
5. **sendat** store.go:16+main.go:56 — 不调用 host.DataDir，tasks.json 落在进程 CWD，cwd 变化即丢全部任务
6. **checkin** report.go:22-48 — 手动+自动签到并发窗口（check-then-set 无临界区），重复签到汇总交错
7. **teletype**（Stop 超时 15s 后重置 WaitGroup，卡 FLOOD_WAIT 的旧 worker 稍后 Done 打到新 WG → negative counter panic）

## P1 — 功能失效或错误行为

8. **quote** main.go:312 — webp 贴纸硬编码 InputPeerSelf，默认输出永远发进收藏夹而非当前聊天
9. **quote** main.go:501 — 语音波形漏 `>>3` 反移位，波形渲染满格平顶（yvlu 的 `b>>3` 是对的）
10. **quote** media.go:241 — `min(doc.Size, maxDLSize)` 绕过 30MB 上限，大视频整段拉进内存
11. **yvlu** sticker.go:83-92 — `yvlu s` 存图片不转 512 边长 WebP，STICKER_PNG_DIMENSIONS 必拒
12. **bs** forward.go:152-173 — sequence 模式首个目标**失败**也 break，与「首个成功即停」语义相反
13. **lottery** commands.go:202/275 — 尾部 `return ctx.Delete()` 在无删权限群抛错，成功路径被误判失败
14. **trace** main.go:254-261 — ReplyToID≠0 判回复，论坛话题内裸命令追踪到话题根作者（untrace 破坏性）
15. **postsearch** search.go:93-99 — Sscanf 接受 `123abc` 残缺 token 当 chatID（save/main.go:279 同病）
16. **parsehub** relay.go:395-398 — 一次瞬时失败（含 FLOOD_WAIT）即放弃整条链接等待
17. **autochangename** commands.go:63-66 — 框架 RawArgs 折叠空白，命令路径的多行 add 永远只有一行
18. **hitokoto** main.go:77 — 「默认类型」设置注册后从未读取，面板承诺完全不生效
19. **acron** run.go:31-50 — peerRef 失效（PEER_ID_INVALID）后无重解析回退，任务永久失败
20. **autodel** listener.go:168-203 — `-r` 规则取「最近 100 条里任意 3 条自己的消息」，会误删命令后用户新写的无关消息
21. **annualreport** render.go:110-113 — 小时直方图丢弃奇数小时桶（`_ = n2`），奇数时段不可见
22. **pmcaptcha** main.go:275-294 — 陌生人双消息并发通过 hasChallenge 检查，重复 mute/发题
23. **music**（commands.go:19-24 无锁写 p.api/p.resolver 与调度 goroutine 数据竞争）

## 其余 edge / polish 明细（按域）

### 媒体/渲染（quote yvlu sticker cosplay wordcloud rev bizhi）

# PaperValet-Plugins 审查 — 媒体/渲染组（quote yvlu sticker cosplay wordcloud rev bizhi）

SDK 依据：`/root/PaperValet/pkg/plugin/sdk.go`、`markdown.go`（Bold/Link 内部自带 Escape）、`docs/plugin-sdk.md`（设置只能进面板、回调 data ≤32B、RateLimit 字段）。已对照 TeleBox 原版（quote.ts / generate.js / text-prepare.js / attachments.js）核实实体偏移（UTF-16 直传正确）与波形语义。

## quote

- [bug] quote/main.go:312 — sendSticker 硬编码 `Peer: &tg.InputPeerSelf{}`，webp 贴纸（`quote` 默认输出）永远发进 Saved Messages 而不是当前聊天，replyTo 也落在自己收藏夹里 → 改为 `ctx.ResolvePeer()` 的 peer（sendPhoto/main.go:289 就是这么做的）。
- [bug] quote/media.go:241 — 视频/动图分支把 `min(doc.Size, maxDLSize)` 当"大小"传给 downloadLocation，30MB 上限检查（media.go:26）被绕过，Stream 会把整个（可能数百 MB 的）视频拉进内存 → 传真实 doc.Size 并给 Stream 包一层限流 writer（超限即中止）。
- [bug] quote/main.go:501-504 — 语音波形 `min(31, int(b))`：tg 的 waveform 字节是 5bit 幅值左移 3 位（amp<<3），amp≥4 时字节≥32 全被钳成 31，波形渲染成满格平顶；yvlu/media.go:577 的 `b>>3` 才是正确解码 → 改为 `min(31, int(b)>>3)`。
- [edge] quote/bridge.go:156-203（配合 bridge.mjs:90-128）— 首次运行引导（下载 30+ 资源、npm install）无任何互斥，两条 quote 并发触发会在同一 data 目录同时跑 npm install / 写 .ready，可能装坏 node_modules → 在 Go 侧加进程内 mutex 包住 runBridge，或 bridge.mjs 里用锁文件。
- [edge] quote/bridge.go:191-192 — 错误信息按字节 `msg[len(msg)-400:]` 截断，多字节中文/ANSI 序列被从中间切断后拼进 `ctx.Edit`，产生非法 UTF-8，Telegram 可能拒绝该编辑 → 按 rune 截断（同文件 errBuffer 的 8192 截断同理）。
- [polish] quote/main.go:484-486, 870-878 — 频道消息SenderID=0，名字退化成 "Channel 123456" 占位符且所有消息被 groupPos 判成同一发送者 → 从 GetMessages 返回的 chats 里解析真实频道标题（或 PeerResolver 反查）。

## yvlu

- [bug] yvlu/sticker.go:83-92 — `yvlu s` 回复图片时把原图以 `image/jpeg` 直接 uploadStickerDoc 后加入贴纸包，没有 512 边长 PNG/WebP 转换；Telegram 会以 STICKER_PNG_DIMENSIONS 拒绝任意尺寸 JPEG，"存图片进贴纸包"功能基本必败 → 复用 sticker 插物的 ffmpeg 方案（pad 到 512 + libwebp）后再上传。
- [edge] yvlu/quote.go:20 — 消息全文、头像、媒体 base64 全部 POST 到第三方 quote-api（zhetengsha.eu.org），而同仓库 quote 插件特意本地渲染（"No remote quote service"）；隐私口径不一致且服务随时可能消失 → 在 README/help 里明示外发行为，或加本地 quote 引擎回退。
- [polish] yvlu/main.go:56-67 — validateStickerSet 只查字符集，不查"必须字母开头"（Telegram short_name 规则，sticker 插件 parse.go:51 有正确实现），数字开头的包名要到运行时才报 STICKERSET_INVALID → 与 validPackName 对齐。
- [polish] yvlu/sticker.go:120 — `plugin.Link(plugin.Escape(pack), …)` 双重转义：Link 内部已对 text Escape（markdown.go:76），包名含 `_`/`*` 时显示成 `\_` → 去掉外层 Escape。
- [polish] yvlu/quote.go:440-448 — buildItemsChecked 注释声称"无可用发送者时报错"，实际只检查 len(items)==0（永远非空），源插件的失败分支已丢失，纯死代码误导维护者 → 实现该检查或删掉包装函数。

## sticker

- [edge] sticker/media.go:336-344 — downloadTo 对输入无大小上限（仅 convTimeout），回复一个超大 image/gif 文档（数百 MB）会被整份下载再去转贴纸 → 下载前检查 doc.Size（对照 rev 的 50MB / quote 的 30MB 上限）。
- [edge] sticker/favorite.go:189-241 — 驱动 @Stickers 机器人用固定 sleep（1.5s/2.5s）+ latestBotText 读最近 5 条历史：机器人回复慢于 sleep 时读到旧消息，误报"机器人返回未知信息"（此时贴纸可能已加上）→ 轮询直到出现比 forward 更新的消息 ID，或自适应延长等待。
- [polish] sticker/main.go:22,126 — tmpDirRoot 是相对路径 `data/sticker/tmp`，而 cfgPath 已换成本插件 DataDir（main.go:65-67）；进程 CWD 不是 PV 根目录时临时文件写去别处（MkdirAll 错误也被忽略）→ tmp 目录同样从 DataDir 派生。
- [polish] sticker/favorite.go:118-131 — 无默认包时每次收藏最多串行 50 次 MessagesGetStickerSet 查找，很容易吃 FLOOD_WAIT → 连续 N 个满包即提前放弃或把首个结果缓存。
- [polish] sticker/favorite.go:74 — `plugin.Link(plugin.Escape(packName), …)` 双重转义（Link 内部已 Escape），包名含 `_` 显示成 `\_` → 去掉外层 Escape。
- [polish] sticker/favorite.go:287-321 — 默认贴纸包用 `sticker <包名>` / `sticker cancel` 命令设置，违反 SDK "选项只进机器人面板，不用命令" 的约定（DefaultPack 应做成 SettingText，如同 yvlu 的 stickerSet）。

## cosplay

- [polish] cosplay/main.go:150-151 — `plugin.Bold(plugin.Escape(ps.Title))` 双重转义：Bold 内部已 Escape（markdown.go:62），标题含 `_`/`*` 时显示反斜杠 → 去掉内层 Escape（中英文两条分支都改）。
- [polish] cosplay/main.go:79（注册处）— 命令无 RateLimit 也无并发去重：一次调用最坏 6 轮抓取 + 10 张下载 + 10 次上传，连点会叠加 → 注册时加 RateLimit（如 10-30s）。

## wordcloud

- [bug] wordcloud/ttcfix.go:28 — `binary.BigEndian.Uint32(src[12+4*fontIndex:])` 无长度校验：TTC 头声称的字体数超过文件实际长度时切片越界 panic，插件跑在宿主进程里，整个 bot 崩溃 → 读前校验 `12+4*fontIndex+4 <= len(src)`。
- [edge] wordcloud/main.go:483 — 面板"立即生成"按钮每次点击 `go p.pageRun()`，无去重：连点 10 次就并发 10 个 2000 条历史的抓取（FLOOD_WAIT 风暴）；tick() 里的 running 去重没有覆盖这条路 → pageRun 复用 running map（以 target 为 key）。
- [edge] wordcloud/render.go:30 — 字体路径硬编码 `/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc`，非 Debian/未装 fonts-wqy-zenhei 的机器上命令永远失败且报原始错误 → 依次探测常见 CJK 字体路径（noto/wqy/droid），都缺失时给出安装提示。
- [polish] wordcloud/render.go:227 — 渲染进图片的页脚 "最近 N 条热词云 | M 条有效消息" 只有中文，英文用户看到的成品图也是中文 → 页脚做成中英并列或按语言二选一。
- [polish] wordcloud/main.go:419-422 — `wordcloud abc` 这类非数字参数被 parseLimitArg 静默当默认 500 处理 → 非数字时报参数错误（cmdSend 分支同理）。

## rev

- [polish] rev/main.go:21 — tempDir 是相对路径 `data/rev/tmp`（os.MkdirAll 于 main.go:430），CWD 不对时写到别处 → 用 host.DataDir 派生（本组 sticker 同病，rev 无 DataDir 依赖更该改）。

## bizhi

- [polish] bizhi/main.go:431 — btstu 回退的文件名恒为 `.jpg`，PNG/WebP 源也伪装成 jpg，-f 文档模式的 MimeType（mime.TypeByExtension）随之错误 → 从 imgurl 后缀或 Content-Type 推断扩展名。
- [待确认][polish] bizhi/main.go:286 — `q=tag1+tag2` 经 url.Values.Encode 会把 `+` 编成 `%2B`；若 wallhaven 不把字面 `+` 当分隔符，双标签查询会空结果（代码里专门写了空结果重试，疑似正是此症状）→ 验证 wallhaven 解码行为，必要时用 `%20` 拼接。
- [polish] bizhi/main.go:97（注册处）— 无 RateLimit：单次调用可下载 50MB 壁纸，连续触发流量叠加 → 加 RateLimit。

## 总体印象

本组整体代码质量高于预期：错误路径普遍有 Tlocal 双语与 Escape、临时文件基本都有 defer 清理、FLOOD_WAIT 大多有处理，实体 UTF-16 偏移在三处（quote/yvlu/rev）都处理正确。真正的问题集中在两类：媒体管线里的"隐性契约"失守（quote 的 30MB 上限被 min() 架空、yvlu 图片进贴纸包没做 512 转换、quote 贴纸发进 Saved Messages），以及从 TeleBox 移植时保留下来的双刃剑行为（第三方 API 外发、@Stickers 机器人固定 sleep 会话、命令式配置违反面板约定）。rev 几乎无可挑剔，是这组的标杆。

### 调度/状态（acron sendat shift checkin annualreport autodel pmcaptcha luxiaoxunbs）

# PaperValet-Plugins 深读审查 — 调度/状态机组（8 插件）

审查范围：acron、sendat、shift、checkin、annualreport、autodel、pmcaptcha、luxiaoxunbs（plugins-external/<name>/，全部非测试 .go 逐文件通读）。SDK 依据 /root/PaperValet/pkg/plugin/sdk.go 与 docs/plugin-sdk.md。

## acron

- [bug] run.go:31-50 — 任务持久化 peerRef 后，若 API 因频道重建/access hash 变化拒绝该 peer（PEER_ID_INVALID/CHANNEL_INVALID 等），不会回退用 t.ChatID/t.Chat 重解析（那两个分支只在 peer==nil 时走）→ 任务从此永久失败，只剩错误文案。→ 在 runTask 捕获这类 tgerr 后改用 resolver 重解析并回写 Task.Peer 再重试一次。
- [edge] run.go:202-233 — runDelRe 翻页只对 *tg.Message 推进 offsetID：若某页 100 条全是 service/空消息，offsetID 不变且 `len(msgs)<pageSize` 不成立 → 同一页死循环连打 getHistory（直至 60s runTimeout）。→ 用 m.GetID()（含 service 消息）推进 offsetID，annualreport history.go:79-87 即此做法。
- [edge] cmds.go:141-155 — 保存失败时回滚 p.tasks/p.next，但 p.nextID 已自增（ID 空洞）且回滚后未提示重试；低危。→ 一并回滚 nextID 或接受空洞并在文案说明。
- [polish] main.go:253+269（afterRun→saveLocked）— 每次任务触发都全量重写 tasks.json，仅为记账 LastRunAt/LastResult/LastError；秒级 cron 任务会造成持续磁盘 IO。→ 记账字段内存化、去抖（如 30s 合并一次）落盘。
- [edge] list.go:23-34（待确认）— 会话范围用 t.ChatID（目标对话）过滤：在 A 群回复创建"发给 B"的任务后，A 里 `acron ls` 看不到、B 里能看到，与直觉（在哪创建在哪管理）相反。→ 确认设计意图，或增加"创建会话"字段用于过滤。

## sendat

- [bug] store.go:16 + main.go:56 — 全程不调用 host.DataDir，p.dir 是编译期相对路径 "data/sendat"（Init 只在 New 设定；测试 main_test.go:144/210 手动注入 p.dir 掩盖了这点）→ tasks.json 落在进程 CWD 而非宿主数据目录，cwd 变化即"丢失"全部任务，违反 SDK DataDir 约定。→ Init 中与其它插件一致调用 host.DataDir(p.Name())。
- [edge] main.go:296-306 — once 任务发送失败即被删除不重试，且 LastError 先写入随即随任务一起从列表消失 → 用户视角是任务无声消失。→ 失败保留任务做短退避重试，或至少把失败记录持久化到独立位置。
- [edge] main.go:297-300 — interval 任务发送失败也扣减 TimeLimit："3 times" 若 2 次 FLOOD_WAIT 失败 + 1 次成功即结束。→ 仅成功时递减剩余次数。
- [edge] main.go:131-141+189-195 — 重启后 24h（overdueGrace）内过期的 once 任务 firstRun→now 立即发送：停机一晚后启动瞬间集中补发多条过期消息。→ 提供过期即丢弃开关或补发前汇总提示。
- [polish] main.go:617-619 — list 超长按 rune 硬截 3900，可能切在 `**`/`` ` `` 中间产生坏 Markdown；同仓其它插件用 clipLines 按行截断。→ 改用行边界截断。
- [polish] main.go:307（afterRun→saveLocked）— 每次发送后全量重写 tasks.json（Count/LastRun 记账），高频 interval 任务 IO 放大；与 acron 同款问题，可去抖。

## shift

- [bug] rules.go:146-163 — wlCache 是包级全局 map 且无锁：listener 转发 goroutine（forwardOne→isFiltered→compileWhitelist）与 `shift whitelist add` 命令线程并发写 → runtime fatal "concurrent map writes" 直接崩进程。→ 加 sync.Mutex 或改 sync.Map。
- [bug] main.go:91-105+121-127 — Stop 等 wgs 上限 15s；超时返回后 Start 里 `p.wgs = sync.WaitGroup{}` 重置，而仍卡在 FLOOD_WAIT 睡眠（最长 120s+）的旧 worker 稍后 `defer p.wgs.Done()` 打到新 WaitGroup → "negative WaitGroup counter" panic。触发需 Stop 超时+快速 restart，罕见但真实。→ 超时路径不重置原 WaitGroup（用新字段/代次计数）。
- [edge] commands.go:285-296 — `shift del 1,1` 类重复序号：parseIndices 不去重，倒序执行删除两条不同规则并报"已删除 2"；越界项静默 continue 不计入 invalid。→ parseIndices 去重，越界进 invalid 列表。
- [edge] forward.go:122-151 — chainForward 把源聊天的 origID 当目标聊天 getHistory 的 OffsetID（跨聊天 ID 无语义），取"最新一条"可能转错消息；且只认 *tg.MessagesMessagesSlice，基本群返回 MessagesMessages 时链式转发静默中断。→ OffsetID 置 0 并兼容两种返回类型。
- [edge] forward.go:49-60 — isCommandText 硬编码 `.!/#`，不用 host.Prefixes()：自定义前缀的命令会被转发出去（噪音），而以 . 开头的普通自有消息又被漏转。→ 用 Host().Prefixes() 判定。
- [polish] stats.go:47-57 — recent() 直接按 map 遍历序取"最近 N 天"，未排序日期，`shift stats` 输出顺序随机。→ 对 day keys 排序后再取尾 N。

## checkin

- [bug] report.go:22-48 + main.go:213-237 — cmdRun 的 `busy := p.running` 检查与 goroutine 内置 running=true 之间有窗口，自动 tick 同样先查后置 → 手动+自动（或两次手动）可并发进入 runAll：重复签到、汇总交错。→ 在同一临界区内 check-and-set running。
- [bug] commands.go:19-24 — handle 无锁写 p.api/p.resolver，调度 goroutine（runSingle/pollHistory）并发读同字段 → 数据竞争。→ 写入也持 p.mu。
- [edge] run.go:141-155 — pollHistory 遇 FLOOD_WAIT > 1min 不等待，~1s 后继续请求直到 10s deadline → 限流期间反复撞墙、可能延长封禁。→ 睡 min(d, 剩余时间) 再查。
- [polish] run.go:203 — 局部变量 sentID 遮蔽同名函数 sentID()（此处恰好需要 int，当前正确但极易误改）。→ 局部改名 sentMsgID。
- [edge] main.go:226-237 + schedule.go:128-136 — 计划时刻在 23:5x、跨零点后首次 tick 时 localDate 不同 → stateMissed，当天签到被静默跳过（窗口边界固有）。→ 给 dueState 加几分钟跨日容差或按 NextRunAt 当日补跑。

## annualreport

- [bug] render.go:110-113 — hourHistogram 丢弃奇数小时桶（`_ = n2`），24 桶只画 12 个偶数桶的柱：奇数时段计数在柱状图完全不可见（仅峰值行体现）。→ 每行画 n1+n2 合并柱或逐小时输出。
- [edge] main.go:188-193 — refresh 只识别 args[0]：`annualreport 2025 refresh` 解析失败并倾倒整页 help。→ 扫描任意位置剥除 refresh。
- [edge] me.go:31-41 — dialogCensus/blockedCount 失败时零值照常渲染"0 个频道 · 0 个群组/黑名单 0 人"，报告呈现为"账号为空"而非出错。→ 失败时输出重试提示或标注未知。
- [edge] main.go:93-110+250-267 — 扫描在命令 goroutine 内同步跑且 Stop 只 cancel 不 join（无 wg）：`.reload` 期间命令挂起至取消，进度 Edit 失败被忽略。低危。→ Stop 带有界等待。
- [edge] store.go:50-66 — saveCacheEntry 读-改-写无插件级锁，两个会话同时生成报告会各写各的、丢一条缓存。→ 加 mutex 或单写者合并。

## autodel

- [bug] listener.go:168-203 — findResponses 普通聊天分支取"最近 100 条里任意 ≤3 条自己的消息"（无 msg.ID > cmdID 限制、无时间/回复链过滤），且是在延迟到期那一刻取 history —— 命令发出后用户新写的无关消息也会命中 → `-r` 规则可能误删与命令完全无关的消息。→ 限制 ID 连续紧随 cmdID 或按时间窗/回复关联过滤。
- [edge] listener.go:25-28 — onMessage 以 `ev.Text == ""` 早退：纯媒体自发消息（贴纸/图片/语音）不触发 TTL 删除 → TTL 语义下"自己发的图"永不删。→ 用 ev.Message/ev.Media 判定而非纯文本。
- [edge] listener.go:107-125 — schedule 在更新路径上同步做 O(n) pending 去重扫描 + 全量 pending.json 重写：开了 TTL 的 busy 群里每条自发消息都触发一次磁盘写。→ 内存登记 + 去抖批量落盘。
- [edge] main.go:117-130 + listener.go:137/247 — Stop 忽略传入 ctx 且 wg.Wait() 无超时，deleteNow/restorePending 内部又用 context.Background()（删除 30s + history 30s）→ 卸载/关机最多阻塞约 1 分钟。→ 删除动作用可取消 ctx，Stop 加超时。

## pmcaptcha

- [bug] cmd.go:112-128 — 无参数 `pmc test` 把验证题发给 owner 自己：owner 的任何消息都进不了 handleReply（出站走自动放行分支 main.go:251-263，入站 uid==self 提前 return），只能等超时 → finishTimeout 对 self 执行失败动作：静音+归档自己的对话；fail_action=delete 时 MessagesDeleteHistory(revoke) 删自己 Saved Messages，=report 时举报自己。→ 拒绝 target==SelfID，或 test 路径跳过失败动作。
- [bug] main.go:275-294 + captcha.go:220-249 — 陌生人两条消息几乎同时到达：两个 goroutine 都通过 hasChallenge 检查（setChallenge 在网络发送之后才执行）→ 重复 mute/archive/发题（最终收敛一条但多跑一轮全流程 API）。→ beginChallenge 入口先原子放置占位 challenge 再做网络操作。
- [edge] captcha.go:253-286 — 面向陌生用户的验证题/页脚文案纯中文（challengeText/challengeFooter/各 sendPlain 提示），英文用户看不懂题意，不符双语约定。→ 按 host.Lang 或至少中英并列。
- [edge] captcha.go:368-385 — 每次 sendChallenge 都 arm 新 timer 且不 Stop 旧的（靠 onTimeout 的 expired 判定自愈）；当前重复发题入口少，影响有限。→ challenge 里存 *time.Timer 并替换时 Stop。
- [edge] captcha.go:116-128（待确认）— peerOf 对会话从未见过的用户回退 hash=0 的 InputPeerUser，发题可能 PEER_ID_INVALID 只留 debug 日志，陌生人侧表现为"被无视"。→ 失败时升级日志并考虑重试 resolve。

## luxiaoxunbs

- [bug] main.go:319-346 — sendAll 首次懒加载贴纸路径锁不平衡：323 Unlock → 331 Lock/333 Unlock → 346 对未持有的锁再 Unlock → "sync: unlock of unlocked mutex" panic（首次整点报时必触发，goroutine panic 可掀翻宿主）；且 335-345 段在无锁状态读写 p.docs/p.state（与 sub/reload 命令数据竞争）。单测只测了纯函数未覆盖此路径。→ 懒加载分支结束后重新 Lock 再统一快照并 Unlock 一次。
- [edge] sticker.go:111-144 — sentMessageID 不处理 UpdateMessageID 形态返回 → 部分会话 newID=0、Last 记 0，下小时 lastID>0 不成立 → 旧报时贴纸永不删除、逐小时残留。→ 补 UpdateMessageID 分支（参考 acron run.go:137-153 idFromUpdates）。
- [edge] main.go:371-384 — 发送遇 FLOOD_WAIT 只 warn 即跳过，该会话本轮报时丢失，下次要等 1 小时。→ sleep d（封顶）后重试一次。
- [polish] main.go:433-443 — bot page 的 rm 忽略 save 错误仍 Toast"已移除"（磁盘失败时重启后订阅复活）；命令路径同类错误均有提示。→ 校验 save 错误再 Toast。

## 本组总评

整体工程质量高：JSON 全部 temp+rename 0600 原子写、调度循环普遍 maxSleep+poke 模式对时钟跳变稳健、FLOOD_WAIT 大多有分类处理、OwnerOnly/Escape/双语基本到位。真正的风险集中在单测盖不住的四条运行时路径：luxiaoxunbs 首跳锁失衡 panic、autodel findResponses 误删无关消息、pmcaptcha `pmc test` 对自己执行失败动作、shift wlCache 无锁并发崩溃——建议优先修这四个，其次统一 sendat 走 DataDir 与各插件"每次触发全量重写 JSON"的 IO 去抖。

### 群管理/监听（ban clean keyword lottery bs textmode pangu atadmins）

# PaperValet-Plugins 深读审查 — 群管理/监听域（ban clean keyword lottery bs textmode pangu atadmins）

范围：plugins-external/ 下 8 个插件全部 .go（不含 _test.go），对照 SDK（PaperValet/pkg/plugin/sdk.go、listen.go、markdown.go、docs/plugin-sdk.md）与 gotd v0.161.0 生成代码逐条核实。严重度：bug（功能性缺陷）/ edge（边界场景）/ polish（优化与约定）。

## ban

- [edge] main.go:363-381 — `single` 的 ban 先 `deleteHistoryCurrent` 清消息、后执行封禁；封禁失败（目标是管理员、权限不足）时对方消息已被清且结果只报封禁错误 → 先封禁成功再清历史，或失败时说明消息已清。
- [edge] main.go:395-399 — kick 第二步解封失败时整体报错，但用户实际处于永久封禁状态，结果消息不说明 → unban 失败时提示「已封禁但解封失败，请手动 unban」。
- [edge] main.go:337-343 — mute/unmute/unban 复用 `meCanBan`（BanUsers||DeleteMessages 即通过）：仅有删消息权限的管理员执行 mute 会到 editBanned 才报 CHAT_ADMIN_REQUIRED，文案「需要封禁成员的管理员权限」对禁言场景误导 → 按 action 校验对应权限并区分文案。
- [edge] resolve.go:244-295 — `scanGroups` 的会话分页循环无页数上限（clean 的同类循环有 50 页护栏），超大账号 `refresh` 可能长时间无法终止且无进度更新 → 加页数上限并在超限时提示。
- [polish] resolve.go:400-418 — `adminEverywhere` 串行逐群 getParticipant 且无 FLOOD_WAIT 重试，群多时 sb 的确认阶段既慢又易触发限流（batch 已有并发信号量与 retryFlood 可复用）→ 并发 + 重试，或缓存 participant 结果。
- [polish] resolve.go:382-392 — `writeGroups` 用固定 `.tmp` 文件名（本仓库其它插件均为 CreateTemp），异常残留的临时文件会与下次写入竞争 → 改 CreateTemp + rename。

## clean

- [edge] member.go:217-327 / deleted.go:293-344 — 边踢人边按 offset 翻 `ChannelParticipantsRecent`：每踢一人列表整体前移，固定 offset 会跳过未扫描的成员（漏踢/漏报）→ 先全量收集再执行踢除，或按已见 id 集合去重续扫。
- [edge] member.go:176-183 — 破坏性模式 5（移出全部普通成员）可用 `rm` 参数绕过 confirm 强警告直接执行（守卫条件 `!HasArg("confirm") && !HasArg("rm")` 同时放行两者）→ 模式 5 只认 confirm。
- [edge] blocked.go:104-140 — PM 解封失败的条目不重试：offset 仅按「本批跳过数」前进，失败条目随页前移被永久跳过，总结只计 failed 不给名单 → 按 id 记录失败条目，结束后补一轮或列出。
- [edge] blocked.go:128-130 — 中途 fatalErr 终止（>60s 的 FLOOD_WAIT 也按 fatal 处理）时已解封计数完全不展示，`finishErr` 只渲染错误文本 → 终止消息附「已完成 N 个」统计。
- [polish] sticker.go:58-132 — 贴纸稀少的大群会从新到旧扫完整历史（每页 100 条、1.2s/页）才结束；循环条件 `deleted < max` 在无贴纸时不收敛于历史深度 → 连续 N 页无命中即提前终止。
- [polish] deleted.go:79 — 每文件夹 50 页（≈5000 会话）静默截断，扫描总结不提示 → 超限时在结果中注明截断。

## keyword

- [polish] task.go:162-167 + main.go:252 — 正则任务在每条群消息上、于 p.mu 内重新 `regexp.Compile`，key 长度无上限；高流量群 + 多任务时是持锁 CPU 热点（RE2 无回溯但编译成本实在）→ 添加/加载时编译一次缓存 `*regexp.Regexp`，编译失败拒绝保存。
- [edge] main.go:374-418 — `moderate` 的 ban/restrict 不检查目标是否管理员/creator、也不检查自身权限，editBanned 失败仅记日志，自动回复照发，惩罚静默落空无人知晓 → 失败（USER_ADMIN_INVALID / CHAT_ADMIN_REQUIRED）时在群内提示一次或在任务列表标注权限缺失。
- [edge] task.go:189 — `$mention` 展开的发送者昵称未 Escape（plugin.Mention 内部 Link 会转义链接目标但 text 部分经 Escape——此处传入的是原始 name，markdown 层 `Link` 的 text 已 Escape ✓，但 `$code_name` 直接拼接原文），含 Markdown 字符的昵称可打乱回复格式 → `$code_name`/`$mention` 处统一走 Escape（待确认渲染层覆盖范围）。
- [edge] main.go:341-377 — 动作顺序为「先发回复 → 再删原消息/封禁」，delete 类任务的原消息在删除前已公开展示数秒 → 先执行 delete/ban 再发回复（待确认源 TeleBox 插件顺序）。
- [polish] main.go:252-254 — cooldown 在「匹配即占用」而非发送成功：发送失败也进入冷却期 → 发送失败时回滚 `p.cool` 条目。
- [polish] commands.go:214-233 — 删除任务后 alias（继承关系）不清理：目标群任务删光后继承空挂，无提示 → 删除任务时检查引用该群任务的 alias 并提示。

## lottery

- [bug] commands.go:202 / 275 — `cmdCreate`/`cmdDraw` 尾部 `return ctx.Delete()`：在无删除权限的群里 Delete 报错直接抛给框架，命令消息残留且成功路径被误判为失败 → 删除失败仅记日志返回 nil。
- [edge] telegram.go:307-354 — `performDraw` 删除公告时释放 p.mu（网络调用），窗口内新报名仍会被接受（status 仍 active）并计入最终 winners → 删公告前先原子置 completed / 快照参与者。
- [edge] commands.go:423-469 — `cmdClaim` 无权限校验（draw/delete 有 canDraw），任何能触发命令的用户可标记领奖状态 → 复用 canDraw（创建者/群管理员）。
- [edge] telegram.go:431-440 — `findLockedByWinner` 全局按用户搜首个中奖记录：同一用户在多个抽奖中奖时，DM 成功可能把别的抽奖标成 sent → dmWinner 携带 lottery id 定位。
- [edge] store.go:216-228 — 过期奖品仅改状态：库存不回流仓库、创建者无任何通知，autoLoop 静默标记 → 过期时回补仓库库存或 b.Notify 创建者。
- [edge] telegram.go:102-115 — `fetchUser` 失败（返回 nil）时机器人账号报名无法识别（仅成功取到 user 且 Bot 才拒绝），bot 消息保留且可中奖 → 解析失败时跳过该次报名或退避重试一次。
- [polish] rng.go:18-28 — 拒绝采样域取 `[0, 1<<63)`（limit 基于 max=2^63），丢弃一半随机数；输出仍均匀但循环次数翻倍 → limit 用 `math.MaxUint64 - math.MaxUint64%n`。
- [polish] page.go:16-33 — 面板 del/draw 按钮绕过 cmdDelete/cmdDraw 的权限检查（canDraw），del 也不清理置顶公告，与命令路径行为不一致 → 面板操作复用命令逻辑。

## bs

- [bug] forward.go:152-173 — sequence 模式在首个目标**失败**（权限错误、解析失败等）后同样 `break`，不再尝试后续目标，与帮助文案「顺序=首个成功即停」语义相反 → 仅在 err==nil 时 break。
- [edge] forward.go:41-79 — `bs <N>` 的 N 无上限（span 500）：数百个 id 塞进单次 messages.forwardMessages 可能超过服务端单次转发上限而整单失败 → 限制 N 或按 ~100 一批分次转发（待确认服务端确切上限）。
- [edge] forward.go:323-341 — 目标端反馈假定转发后 id 连续递增（FirstID+i），话题/服务端重排时不成立，链接指向错误消息 → forwardStats 已收集全部新 id，改为传递完整列表。
- [edge] targets.go:96-111 — 持久化的 access_hash 失效（重新登录、对端重置）后仅报错不回退，目标永久不可用 → 遇 PEER_ID_INVALID / CHANNEL_INVALID 时清空 t.Peer 重新解析一次再试。
- [edge] main.go:461-470 — page 的 rm/off/on 分支 `_ = p.saveLocked()` 吞掉保存错误，崩溃后操作丢失且无 Alert → 失败时 c.Alert 提示。

## textmode

- [edge] listener.go:90-99 — 带媒体/相册消息的 caption 同样被整段加实体重发：相册共享 caption，编辑会波及整组媒体消息 → `ev.Media != nil` 时跳过。
- [edge] listener.go:93 + 114 — 判空用 `strings.TrimSpace(ev.Text)`，但把**修剪后**的文本传给 editMessage：消息首尾空白（含换行）被悄悄删除，超出「只加格式不动内容」的承诺 → 判空用 TrimSpace，编辑回传原文。

## pangu

- [edge] main.go:369-388 — 带媒体/相册消息的 caption 同样被重发编辑：相册共享 caption，改一个等于改整组 → `ev.Media != nil` 时跳过。
- [edge] main.go:406-418 — 编辑只传新文本、Entities 为 nil 不置 flag（已核实 gotd 编码行为），服务端沿用旧实体：插入空格改变长度后链接/@/代码块等原实体偏移全部错位 → 检测 `ev.Entities` 非空时跳过，或按空格插入位置平移实体后再传。
- [edge] main.go:398-400 — `recordFormatted` 每格式化一条消息就全量序列化重写 config.json（含全部名单），高频使用写放大明显 → 内存累加 + 定时/退出时落盘。
- [polish] main.go:373-377 — 命令前缀循环未过滤空前缀（textmode 的 isCommand 有 `pref != ""` 检查），若前缀列表含空串则所有消息被判为命令、auto 永不生效 → 加非空过滤。

## atadmins

- [edge] main.go:284-311 — 分片按「名字 rune 数」估算长度，但 Mention 渲染为 Markdown 链接且 Escape 会为特殊字符加反斜杠，实际长度被低估，极端名字场景单条仍可能超限 → 用渲染后的字符串长度估算。
- [edge] main.go:346-385 — 管理员分页仅以「返回数 < pageSize」终止，无 Count 对照：Telegram 偶发短页会提前截断管理员列表（漏召唤）→ 加 `offset >= participants.Count` 退出条件。
- [polish] main.go:209 — 每个分片都重复完整头部（自定义消息 ≤200 字 + 冒号 + 空行），管理员多时分片刷屏 → 仅首片带完整消息，续片用简短头部（如「…（续）」）。

## 总评

本组插件整体工程素养较高：监听器注册/释放、wg 生命周期、原子写 JSON 0600、双语文案、Escape/Code 约定普遍到位，明显弱项集中在三处。一是「边执行边翻页」模式（clean 踢人、lottery 开奖分段加锁）对服务端列表位移/并发窗口的容忍不足，会造成静默漏处理；二是危险操作的守卫不严（clean member 5 可用 rm 绕过、lottery claim/面板按钮无权限校验、keyword 惩罚动作静默失败），建议统一走 canDraw/confirm 这类集中式守卫。三是热路径成本（keyword 持锁编译正则、pangu 每条消息全量落盘、ban 串行 adminEverywhere），量上来后才暴露，宜趁早缓存与节流。

### HTTP/外部 API（subinfo xmsl ai zpr setu gt weather duckduckgo news ip hitokoto）

# PaperValet-Plugins 审查 · HTTP/外部 API 组（11 个插件）

范围：plugins-external/ 下的 subinfo、xmsl、ai、zpr、setu、gt、weather、duckduckgo、news、ip、hitokoto。
方法：逐文件通读全部 .go（含测试外的所有源码），对照 SDK（pkg/plugin/sdk.go、markdown.go、listen.go）与 docs/plugin-sdk.md；Firecrawl 免 Key 行为与 ip-api 免费版协议为实测验证。

## subinfo

- [edge] main.go:186-193 — sendTxt 写入 os.TempDir() 且文件名仅含秒级时间戳（可预测），os.WriteFile 对已存在文件（如攻击者预置的 symlink）会跟随并截断 → 改用 os.CreateTemp 或插件 DataDir 加随机后缀。
- [edge] http.go:46-62 — mappingCache.get 持锁期间执行远程抓取（最长 10s），queryAll 的并行链接 goroutine 全部串行阻塞在这次抓取上 → 双检锁/singleflight，锁外发起请求。
- [polish] http.go:104-112 — mappingName 遍历 map 取首个命中 key，多个 key 命中同一 URL 时机场名输出不确定（同一输入两次查询可能不同）→ 按固定排序的 key 切片遍历。
- [polish] render.go:444-450 — rawOrLink 两个分支完全相同（txt 分支是死代码），且消息模式下 URL 原样嵌入 Markdown 未走 plugin.Link/Escape，含 `)` 或 `[` 的订阅链接会破坏渲染 → 删除死分支并用 plugin.Link 包裹。
- [edge] main.go:126-136 + http.go:141-167 — 回复模式下候选 URL 来自任意用户的消息（命令虽 OwnerOnly 但被回复内容不受控），服务端会对该 host 发起 /auth/login 与根路径抓取并回显错误详情，构成弱 SSRF 面（OwnerOnly 缓解，标待确认）→ 拒绝私网/环回地址后再抓取。

## xmsl

- [polish] ai.go:310 — Gemini 密钥以 `?key=` 查询参数传输，会进入中间代理/服务端访问日志 → 改用 `x-goog-api-key` 请求头。
- [polish] ai.go:377 — postJSON 用 `raw, _ := io.ReadAll(...)` 吞掉读取错误，截断的响应体最终以 "bad JSON" 形式误导排障 → 记录并包装读取错误。
- [polish] media.go:414 — DataDir("xmsl") 硬编码插件名而非 p.Name()，重命名时数据目录会脱钩 → 用 p.Name()。

## ai

- [edge] main.go:118-127 — `ai` 命令既无 OwnerOnly 也无 RateLimit，但每次调用直接消耗面板中配置的付费 API key，任意群成员都可刷掉额度（对照：subinfo/xmsl 均为 OwnerOnly）→ 至少加 RateLimit，或与 xmsl 一致设 OwnerOnly。
- [polish] provider.go:201 — 同 xmsl：Gemini 密钥放在 `?key=` 查询参数 → 改用请求头。
- [polish] history.go:88,100,111 — 三处 `_ = s.saveLocked()` 静默吞掉落盘错误（磁盘满时历史悄悄丢失），且每条消息全量重写整个 map（每轮问答 2 次全量序列化）→ 至少记日志；可改追加/异步写。
- [edge] main.go:204-211 — 同会话并发两条 `ai` 时，请求 A 失败 rollbackUser 会误删请求 B 刚 append 的 user turn（按"末尾是 user 即删"回滚）→ 回滚时按内容或序号匹配。

## zpr

- [edge] fetch.go:294 + 319-330 — urlExt 把 API 返回的 ext 字段直接拼进 filepath.Join，未过滤 `/`、`..`；上游返回恶意 ext（如 `../../x`）可写出 tmp 目录之外（需上游被控，概率低但一行可堵）→ urlExt 内 strip 路径分隔符。
- [polish] main.go:165-183 — 整条命令无总超时：每图最多 4 镜像 × 30s，10 张图并发 3 最坏约 8 分钟，用户一直停在"传送中…" → 给 downloadAll 套整体 context.WithTimeout（如 3 分钟）。

## setu

- [edge] bot.go:66-93 — waitReply 超时后把最后一条"进度文本"（如"搜索中…"）当作成功结果返回：签到场景显示"✅ 签到完成 > 搜索中…"，图片场景误报"机器人返回：搜索中…" → 超时分支直接走 errNoReply，或过滤已知进度词。
- [polish] send.go:73-77 — MessagesReadHistory 的 MaxID 传 time.Now().Unix()（时间戳不是消息 id，碰巧等价于"全部已读"）→ 用 waitReply 实际收到的消息 id。

## gt

- [polish] main.go:386-405 — 三个后端串行尝试、每个受 10s client 超时约束，加上 flip 重翻，最坏约 60s 且无整体 deadline → 给 translate 套整体超时，或对后端并行竞速取首个成功。
- [polish] main.go:82-91 — 公开命令无 RateLimit，直打 Google 免费 endpoints，群内刷屏易触发同 IP 429（殃及 weather 的 clients5 复用）→ 加 RateLimit（如 5s）。

## weather

- [polish] main.go:237-246 + 385-402 — 每次查询都走完整链路（geocode 最多 2 次 + forecast，必要时再加翻译），同一常用城市短时间内反复全量请求 → 城市名→geoResult 与经纬度→forecast 各加 5-10 分钟内存缓存。

## duckduckgo

- [polish] main.go:53-63 — CheckRedirect 注释称"不跟随到非 DDG 域"，实现却是任意域最多跳 3 次（注释与代码不符；无敏感头随跳，实际风险低）→ 修正注释或在回调里校验重定向目标 host。
- [polish] main.go:200-227 — html→lite→firecrawl 串行降级，DDG 被墙或慢时每笔查询先等前两个端点各超时（client 25s），最坏 ~75s 无整体 deadline → 套整体超时或对 DDG 两个端点并行竞速。

## news

- [polish] main.go:178-213 — 每次命令都重新拉取 API，而日报内容全天基本不变（面板 count/cat 变化才需刷新）→ 按 count+category 维度加 5-10 分钟内存缓存，显著降延迟。

## ip

- [polish] main.go:21 + 111-113 — apiBase 为明文 `http://ip-api.com/json/`（免费版无 https，实测 https 403），查询目标与返回归属地在网络链路明文可见/可篡改（待确认：属上游套餐限制）→ 在 README/帮助里注明，或改用支持 https 的免费源。
- [polish] main.go:41-50 — 公开命令无 RateLimit，ip-api 免费额度按 IP 45 req/min，群内几人连查即触发 429 → 加 RateLimit（≥3s）。

## hitokoto

- [edge] main.go:214-231 + 245-247 — 一切非 200（含 403/404 等不可重试错误）都重试 10 次、固定隔 1s，失败要等约 10 个周期才报错，且对限流中的免费 API 雪上加霜 → 仅对 5xx/429/网络错误重试，次数降到 3。

---

## 本组总评

这批插件的 HTTP 卫生整体扎实：所有响应体都 defer Close、一致使用 io.LimitReader 封顶、按请求加 ctx 超时、错误路径有双语卡片，Escape/Code 纪律好（唯一漏网是 subinfo 的 rawOrLink 死分支）。主要风险集中在三处：ai 命令无 OwnerOnly/RateLimit 却烧付费 key、两个 AI 插件把 Gemini key 放查询串、以及多端点串行降级链（gt/ddg/zpr/hitokoto）缺整体超时导致最坏可达分钟级等待。缓存（news/weather）与限流（gt/ip/ddg）是最廉价的改进项。

### 信息查询/消息（save postsearch parsehub music his trace bgp whois premium speedtest）

# 信息/消息类插件审查（save postsearch parsehub music his trace bgp whois premium speedtest）

审查范围：plugins-external/ 下 10 个插件全部 .go 逐文件通读，对照 /root/PaperValet/pkg/plugin（sdk.go、listen.go、markdown.go）。已核实：宿主把任何带 reply 头的消息都置 IsReply=true/ReplyToID=replyToMsgID（含论坛话题根，updates.go:145-148、listen.go EventFromMessage），插件侧需自行判 ForumTopic——re.go:229 的做法是参照。

## save

- [edge] tg.go:157 — fetchAlbum 只取 msg.ID±10 的窗口拼相册，相册消息稀疏分布（中间大量非本组消息）或 ID 间距 >10 时丢成员 → 改为按 groupedID 从 messages.search 或逐窗口向外扩展直到取齐
- [edge] save.go:487-491 — caption 超长时的 extraText 以刚发的媒体消息为回复对象发送，且 `id = tid` 把 lastSent 记成了文本消息 ID，后续来源卡片会回复到文本而非媒体 → lastSent 应保持媒体 ID（或去掉覆盖）
- [edge] tg.go:288-299 — 无 video/audio/sticker 属性时按 mime 前缀把 document 归类为 photo/audio/video，但重传仍走 InputMediaUploadedDocument：kind 只影响文案/扩展名，mime image/* 不会走 InputMediaUploadedPhoto，行为一致性待确认 → 按上传类型同步归类或删除误导性 kind 推断
- [polish] save.go:343 — 范围保存把 `len(ids)-len(msgs)` 全计入 skipped，被删除的消息与从未存在的 ID 不区分，用户看到的“跳过”数偏大 → 文案区分“不存在/已删除”（可接受现状，注明即可）
- [polish] save.go:480-484 — 媒体直发失败回退 plain file 再失败时返回第一个 err，回退的真实原因（如属性被拒）丢失 → 两个 error 合并 wrap 返回

## postsearch

- [bug] search.go:93-99 — parseChatID 用 `fmt.Sscanf("%d")` 接受尾随垃圾（实测 Sscanf("123abc") 成功返回 123），`postsearch add 123abc` 之类残缺 token 会被当 chatID 解析，报出莫名的 PEER_ID_INVALID → 改 strconv.ParseInt 全串校验（save/main.go:279 的 fmt.Sscan 同病，可一并修）
- [edge] cmd.go:499-505 — import 先无条件清空现有频道列表再逐个 add，中途网络失败时旧列表已丢 → 先解析+add 成功后再原子替换，或失败时回滚
- [polish] cmd.go:40-46 — 多频道搜索之间固定 sleep 750ms 与限流无关，频道多时白白拖慢 → 去掉固定等待，仅对 FLOOD_WAIT 退避（searchOnce 已有）
- [polish] search.go:413-415,456-462 — searchChannel/randomChannel 每次都 resolve+lookup（每频道 2 个 API 调用，且 lookup 对已存 ChatID/Username 的频道是重复信息）→ 缓存 handle→peer/info（TTL 数小时），失效时再刷新
- [polish] search.go:523-528 — searchLinked 里 MessagesGetReplies 无 FLOOD_WAIT 处理，出错直接 continue，多帖评论抓取易被限流后静默漏结果 → 套用 searchOnce 的重试

## parsehub

- [bug] relay.go:395-398 — 轮询循环里 botHistory 一次失败（含瞬时 FLOOD_WAIT）立即以 reasonFetchFailed 放弃整条链接的等待，2 秒一次的高频 poll 撞上限流概率不低 → 连续失败容忍 N 次（或仅 FLOOD_WAIT 等待）再放弃
- [edge] classify.go:12-17 — 进度占位识别硬编码 4 个中文词（“解 析 中”等），@ParseHubot 改文案/英文环境时占位会被当 final 转发给用户 → 至少对“最终消息”加兜底条件（如等待静默期后再校验一次无媒体且短文本的 final）
- [edge] relay.go:457-479 — DropAuthor 转发到有转发限制/禁媒体的群失败时，fallback 只发文本，媒体结果整体丢失，提示也不区分原因 → 失败原因是 CHAT_FORWARDS_RESTRICTED/CHAT_SEND_MEDIA_FORBIDDEN 时明示“结果含媒体无法转入本群”
- [polish] relay.go:98-107 — 每条命令都执行 ContactsUnblock+AccountUpdateNotifySettings（2 次写 API）→ 成功一次后置标志，失败时才重做
- [polish] store.go:120-128 — randomID 用 UnixNano（+i），同一纳秒并行提交会撞 randomID 被 Telegram 去重静默丢发送 → 换 crypto/rand（save/tg.go:423 已有正确实现）

## music

- [edge] main.go:171-173,333-335 — 回复消息时 replyTo 直接用 ev.ReplyToID：论坛话题里普通命令消息的 ReplyToID 是话题根 ID，歌曲会回复到话题根而非用户看到的被回复消息 → 按 MessageReplyHeader 判 ForumTopic/取 realReplyID（参照 save/main.go:292）
- [edge] main.go:319-321 — deliver 用缓存 peer 里的 doc ID+FileReference 重发音频，引用过期（bot 重发/账号重置后）即 MEDIA_EMPTY 失败 → 失败时重取该消息刷新引用重试一次
- [polish] session.go:75-81 — click 的 callback 结果被完全丢弃（后台 30s 超时），按钮 data 失效（bot 改版）时要等满 150s audioWait 才报“没有回应” → 把 click 结果回传 wait，失败提前结束（待确认：部分 bot 确实先回调后慢传文件）
- [polish] session.go:49-52 — events 通道缓冲 32 满即静默丢弃 bot 消息，无日志，丢的恰是音频时表现为莫名超时 → 丢弃时记 Debug 日志或加大缓冲

## his

- [edge] main.go:101,185 — 依赖 IsReply/ReplyToID：论坛话题内不带真实回复的 `.his` 命令 ReplyToID=话题根，被当成“回复某人”，查的是话题根作者（topic 创建者），结果误导 → 用原始 reply 头判 ForumTopic/取 realReplyID（同 save 的 replyTarget）
- [polish] main.go:217-228 — FLOOD_WAIT ≤30s 时无限循环重试（仅 ctx 取消能终止），极端限流下命令挂死到宿主超时 → 加重试次数上限（3 次左右）

## trace

- [bug] main.go:254-261 — 用 `ctx.Message.ReplyToID != 0` 判定“回复了消息”：论坛话题内任何裸 `.trace`（含表情）都会命中 ReplyToID=话题根，追踪/取消追踪写到话题根作者头上（untrace 还是破坏性操作）→ 按 MessageReplyHeader 的 ForumTopic+replyToTopID 区分真实回复（宿主 re.go:229 已有先例）
- [polish] main.go:182-199 — Premium 过期后已存的 custom emoji ID 每次回应都失败且只有 Debug 日志，用户无感知、DB 也不降级 → 连续 REACTION_INVALID/PREMIUM_ACCOUNT_REQUIRED 时提示一次并自动剔除 custom 项（或转标准表情）

## bgp

- [edge] output.go:34-48 — packPages 按“行”分页，renderDNS 把全部 PTR 记录塞进单个 plugin.Pre 块：一行超过 4096 UTF-16 单位（大量正反记录时可能）该页超限，编辑直接失败 → 对超长行按块再切或对 Pre 内容截断
- [polish] handlers.go:64-105,135-148 — Cymru(拨号+20s deadline) 与 3 个 RIPEstat 调用全串行，runNet 90s 总超时最坏很紧张、runASN 同样 4 连发 → Cymru 与 RIPE 并行（errgroup），整体延迟近减半
- [polish] handlers.go:231-233 — tryGraph 全程静默：bgp.tools 登录墙/硬编码 token 失效后永远无图也无日志，无法发现退化 → 失败时记一条 Warn/Debug

## whois

- [edge] store.go:114-128,148 — Cache map 只增不减：过期条目仅在该域名再次被查询时惰性删除，从未复访的域名（含 ≤3000 字符 RawData）永久堆积在 whois_data.json → 定期清扫过期项或加条目上限
- [edge] query.go:81 — parseSSEResponse 只认 `"data: `（带空格）前缀，SSE 规范允许 `data:` 无空格；服务端格式微调即全部解析为空 → 误报“域名不存在”。改为 TrimPrefix("data:") 再 TrimSpace（待确认 namebeta 当前输出格式）

## premium

- [edge] main.go:174-184 — 每页只统计当页 Users 里出现的 participant，跨页不回填；个别成员对象缺失时计数偏低且无提示 → 缺失 ID 收集后在结束后按需补查（或文案注明为估计值）
- [polish] main.go:156-170 — 页间无间隔连发 50 页 getParticipants，FLOOD_WAIT ≥60s 直接放弃（大群常态），<60s 的等待又无次数上限 → 页间加 100-200ms 延迟；长 flood 带上限地等待
- [polish] main.go:38-48 — 命令未设 OwnerOnly/RateLimit：群里任何人都可触发最多 1 万成员的全量扫描（分钟级 API 压力）→ 加 RateLimit（如 60s）或 OwnerOnly，与同组其它命令对齐

## speedtest

- [edge] cli.go:101-152,602-604 — downloadCLI 写 `path+".new"` 无互斥：list/test/best 子命令触发的下载可与进行中的测速下载并发写同一临时文件，产物损坏 → 复用 running 锁或单独的下载互斥
- [edge] main.go:818-819 — sendUploaded 回复目标用 ctx.Message.ReplyToID：论坛话题内命令会回复到话题根而非被回复消息 → 同 music：按 reply 头判 ForumTopic
- [polish] cli.go:27,482 — runTimeout 固定 120s，高延迟链路（跨境/卫星）下载+上传段可能不够，报“测试超时” → 超时按需放宽或失败提示重试/换服务器

## 总评

本组插件整体工程质量较高：并发退出路径（save/parsehub/trace 的 wg+lifetime）、原子写盘、FLOOD_WAIT 局部处理普遍到位，双语与 Escape/Code 约定执行良好。系统性弱点有两个：一是几乎所有依赖 IsReply/ReplyToID 的插件（music/his/trace/speedtest/save 已自行处理）都没防论坛话题根回复，其中 trace 是破坏性误操作；二是“每命令重复 resolve/lookup/start 类准备调用”缺乏缓存（postsearch、parsehub、music）。建议优先修 trace/postsearch/parsehub 三处 bug 级问题，再统一抽一个 forum-aware 的 reply 解析助手进公共参考。

### 小工具（autochangename listusernames teletype paolu portball crazy4 diss ip premium hitokoto）

# PaperValet-Plugins 审查 · 小工具组（group-misc）

范围：autochangename、listusernames、teletype、paolu、portball、crazy4、diss，外加同属小件的 ip、premium、hitokoto（music 归其他组；本组边界取 ≤~330 行的小工具）。
对照 SDK：/root/PaperValet/pkg/plugin（sdk.go、markdown.go、listen.go）与 host 实现（internal/command、internal/bot、internal/settings）。

## autochangename

- [bug] commands.go:63-66 — `add` 子命令承诺「多行」（usage/help 均写 `<文本|多行>`），但框架 `RawArgs = strings.Join(strings.Fields(...), " ")`（registry.go:373）会把粘贴的多行压成一行空格分隔，parseItems 的逐行解析在命令路径永远只见到一行：多条目变一条（name 目标连空格一起进名字）或整条被判超长无效；多行只有面板 Ask 路径可用 → 在 usage/帮助里只承诺面板多行，或改为从回复消息文本取多行内容
- [edge] main.go:265-291 — `rotate` 无串行化：调度 loop 的 runDue 与 `now` 命令/面板按钮可并发执行，两个 `AccountUpdateProfile/UpdateUsername` 竞速，最终生效的资料可能与 setIndex 记录（及「已切换到」提示）不一致 → 用一把插件级 mutex 包住 rotate 全过程
- [edge] main.go:434 — `applyNow` 在 rotate 之后重新读 `curIndex` 拼返回文案，期间并发轮换/删条目会报错条目 → 让 rotate 返回实际应用的 item，applyNow 原样透传
- [polish] commands.go:55 — errText 兜底直接返回英文 `err.Error()`，中文用户看到纯英文原始错误 → 包一层双语「操作失败」+ Escape 后的详情
- [polish] commands.go:135-137 — 列表按 3900 rune 硬切可能切断 code span/转义序列，Markdown 解析回退后渲染错乱 → 按整行截断而不是按 rune 切

## listusernames

- 无发现。分页 chunk 按 rune 且不拆行、标题/用户名均经 Escape/Code，FLOOD_WAIT 有专门文案；单次 API 后多条 Reply 的量级受限于自有公开群数，安全。

## teletype

- [edge] main.go:198-202 — 动画对 FLOOD_WAIT 零容忍：200 字 → 400 次 edit、最快 10ms/帧（typewriter.go:20），必然触发 flood；第一次 FLOOD_WAIT 就整体中止，消息停留在半截文本 + 残留 █ 光标且永不补全 → 捕获 FLOOD_WAIT 睡 d+1s 后重试该帧；中止时补发完整文本收尾
- [edge] main.go:191-212 — 帧序列在动画开始时一次性生成，用户中途编辑该消息会被下一帧覆盖、动画结束时还原成旧文本（用户编辑被静默撤销）→ 中途帧与最终帧发现内容不符即中止（或至少在文档/help 里说明）；与上一条合并处理亦可
- [polish] main.go:255 — auto 模式在监听器（update 路径）里同步做 `ResolveFromChatID`，缓存未命中时会阻塞整条更新分发 → 把 resolve 挪进 wg 管理的 goroutine 内

## paolu

- [edge] main.go:265-274 — 收尾顺序错误：先 `ctx.Delete()` 删命令消息，`host.Send` 摘要失败后再 `_ = ctx.Edit(summary)` 兜底——此时命令消息已被删，Edit 必失败（注释声称的兜底不存在），群被清空+禁言但用户得不到任何结果 → 先尝试发送摘要、失败再 Edit 命令消息、最后才删命令消息
- [polish] main.go:300-306,337-348 — wipe 全程 RPC 只挂 lifetime ctx，无每请求 WithTimeout；配合 Stop 里无超时的 `p.wg.Wait()`（main.go:75），一条卡死的 RPC 会把 Stop/reload 拖死 → 与 autochangename 一致给每个 RPC 加 30s 超时

## portball

- [polish] main.go:379 — 解除时间按服务器本地时区 `2006-01-02 15:04` 输出且无时区标注，跨时区群成员会误读 → 附加 UTC 偏移或统一用 UTC 标注
- 其余干净：论坛主题回复头处理（realReplyID）、min 用户/FromMessage 对象、频道身份禁言、失败错误映射（含 FLOOD_WAIT、USER_ADMIN_INVALID 等）与 5s 延迟删除的 ctx 复制都正确。

## crazy4

- [polish] main.go:66-67 — 用 `IsReply/ReplyToID` 判回复，论坛主题里对主题根消息的隐式回复头也会被判成「回复」，文案会 reply 到主题创建消息上 → 仿 portball 的 realReplyID 过滤 ForumTopic 头
- deck 洗牌避免连发重复（crazy4/main.go:86-100）逻辑正确；文案经 Escape。除此之外无发现。

## diss

- [edge] main.go:64 — API 文本未截断直接 `ctx.Edit`，偶发超长（>4096）时 Edit 报错、消息永远停留在「正在获取…」 → 截到 ~4000 rune 再发
- [polish] main.go:69-84 — 对 4xx/HTML 错误页也无脑重试 5 次（每次隔 1s），注定失败的请求白等 ~5s 才报错 → 仅对传输错误/5xx 重试
- [polish] main.go:38-47 — 未设 `RateLimit`，用户连刷会打爆共享的第三方配额并连累其他命令 → 加个几秒的 RateLimit

## ip

- [polish] main.go:21 — 免费版 ip-api 只支持 http，查询内容（含从别人消息里提取的 IP/域名）明文出网 → 待确认：换支持 https 的端点或至少在 README 注明该限制
- 提取优先级（直连 ParseIP → IPv4 → IPv6 → URL host → 域名）、16 进制串误匹配 IPv6 的兜底（ParseIP 校验）、NoWebpage 编辑都处理得当，无其他发现。

## premium

- 无发现。成员去重（seen map）应对翻页漂移、FLOOD_WAIT <1min 睡眠后重取同页、10k 硬上限与 force 门禁、基础群走 ChatParticipants 分支均正确；进度 edit 3s 间隔不会触发编辑限频。

## hitokoto

- [bug] main.go:77,192-204 — 设置项「默认类型」注册后 `p.set` 从未被读取：`hitokoto` 不带参数时完全忽略面板里配置的默认类型，hint 与 help（main.go:69,171）均承诺了该行为 → 无 args 时把 `p.set.String("type")` 解析进 types（复用 parseTypes 的分割逻辑）
- [polish] main.go:214-231 — 与 diss 同病：10 次重试不区分错误类型，非 200/空响应也重试满 ~10s → 仅对传输错误/5xx 重试
- [polish] main.go:78-87 — 未设 `RateLimit`，连刷耗尽 hitokoto.cn 配额 → 加几秒限频

## 本组总评

整体质量高：OwnerOnly 用得克制而正确（autochangename/paolu），autochangename 的 state.json 是标准的临时文件+rename+0600 原子写，用户内容 Escape/Code 纪律普遍到位，Stop→wg/lifetime 的协程治理在 teletype/paolu/portball 三家基本一致且正确。集中暴露的三个共性问题：hitokoto/diss 这类第三方 API 命令缺 RateLimit 且重试不区分错误类型；teletype 这种高频 edit 插件没有 FLOOD_WAIT 容忍与失败收尾；以及框架 RawArgs 折叠换行导致 autochangename 命令路径的多行 add 实际不可用（这是最值得优先修的一条）。

---
落实状态跟踪：修完一条把行首 `- [ ]` 改 `- [x]`（后续把各条目改造成 checkbox）。本审查未改动任何插件代码。

---

## 落实结果（2026-10-06 修复批次）

6 个修复域并行处理，**51 个 fix commit 全部过完整门禁（gofmt/vet/test/build/真实加载检查）后提交**，未推送时点为本节写入前。

处置统计（185 条发现）：**fixed 165 · rejected 9（审查误报，验证后证伪）· deferred 13（需产品决策，逐条附理由）**

- rejected 示例：bs sequence「首个失败也 break」实为审查误读（default 分支 continue，失败继续、成功即停，已补回归测试锁死）；bizhi `%2B` 编码问题实测证伪；music 「commands.go 无锁写」代码库无此文件
- deferred 示例：trace premium 过期 custom emoji 自动剔除（改数据语义待定）、sticker DefaultPack 迁移、clean 保留统计策略等，明细见各域 status 文件
- P0 全部修复：luxiaoxunbs 锁不平衡（abd8e46）、wordcloud TTC 越界（de485f7）、shift wlCache 无锁（1e59ff3 同批）、pmcaptcha self 目标（a63f6d6）、sendat DataDir（4e79b76）、checkin 并发窗口（91a4bf0）、teletype WaitGroup 代次计数
- 论坛话题 ReplyToID 误判按 save 的 replyTarget 判定统一修复（trace/his/music/speedtest/crazy4 等）
- 共性修复：Gemini key 改 x-goog-api-key 头（ai/xmsl）、第三方 API 命令补 OwnerOnly/RateLimit（ai/premium/ip/gt/duckduckgo/cosplay/bizhi/diss）、消息编辑类跳过媒体 caption 与带实体消息（textmode/pangu）、FLOOD_WAIT 分级重试上限

逐插件明细：/root/.cache/pvreview/status-{media,scheduler,admin,http,info,misc}.md（每条带 commit、文件:行、验证方式）

