# PaperValet-Plugins

PaperValet 外部插件仓库 — 所有第三方插件源码统一维护在这里，主仓库不存放插件源码。

## 内置插件（编译进主程序，不在本仓库）

| 插件 | 命令 | 说明 |
|------|------|------|
| core | version/ping/restart | 核心命令 |
| help | help | 帮助系统 |
| status | status | 运行状态 |
| apt | apt | 插件包管理器 |
| info | info/fwd | 信息查询与转发 |
| alias | alias | 命令别名 |
| exec | exec | 执行系统命令 |
| sudo | sudo | 权限委派 |
| reload | reload | 外部插件热重载 |
| log | loglevel/sendlog | 日志级别与发送 |
| prefix | prefix | 前缀管理 |
| backup | backup | 备份与恢复 |
| update | update/autofix | 代码同步与修复 |
| dme | dme/dme all | 消息清理 |
| lang | lang | 语言切换 |

## 外部插件（动态加载 .so）

| 插件 | 命令 | 说明 | 安装 |
|------|------|------|----------|
| account | username/name/bio/rmpfp | 账号资料管理 | `apt install account` |
| atadmins | atadmins | 一键艾特全部管理员 | `apt install atadmins` |
| bizhi | bizhi | 随机壁纸 | `apt install bizhi` |
| calc | calc | 计算器 | `apt install calc` |
| duckduckgo | ddg | DuckDuckGo 搜索 | `apt install duckduckgo` |
| encode | encode | 编码/解码 (base64/url/hex) | `apt install encode` |
| fun | roll/coin/choose/8ball/fact | 娱乐命令 | `apt install fun` |
| gt | gt | 谷歌翻译 | `apt install gt` |
| hitokoto | hitokoto | 随机一言 | `apt install hitokoto` |
| ids | ids | 显示用户/群组/消息 ID | `apt install ids` |
| isalive | isalive | 检测 bot 运行状态 | `apt install isalive` |
| ping | ping | 网络延迟测试 (TCP/HTTP/ICMP/DC) | `apt install ping` |
| qr | qr | 二维码生成 | `apt install qr` |
| qrcode | qrcode | 二维码生成/解码 (完整版) | `apt install qrcode` |
| re | re | 消息复读机 | `apt install re` |
| rev | rev | 文本反转 | `apt install rev` |
| save | save | 突破限制保存/转发消息 | `apt install save` |
| sendat | sendat | 定时消息发送 | `apt install sendat` |
| speedtest | speedtest | 网络速度测试 | `apt install speedtest` |
| tpm | tpm | 旧版插件管理器 | `apt install tpm` |
| weather | weather | 天气查询 | `apt install weather` |

## 使用方法

### 安装插件
```bash
apt install atadmins ids calc
```

### 加载插件
```bash
apt load atadmins
```

### 查看已安装
```bash
apt list
```

### 搜索插件
```bash
apt search 翻译
```

### 卸载插件
```bash
apt remove gt
```

## 开发外部插件

参考 `plugins-external/atadmins/main.go` 模板：

```go
package main

import (
    "context"
    "github.com/TiaraBasori/PaperValet/pkg/plugin"
)

var PluginMetadata = plugin.PluginMetadata{
    Name:        "your-plugin",
    Version:     "1.0.0",
    Description: "插件描述",
    Author:      "YourName",
    MinVersion:  "1.0.0",
    Commands: []plugin.CommandMetadata{...},
}

type YourPlugin struct{}

func (p *YourPlugin) Name() string        { return "your-plugin" }
func (p *YourPlugin) Description() string { return "插件描述" }
func (p *YourPlugin) Init(ctx context.Context, mgr plugin.Manager) error { ... }
func (p *YourPlugin) Start(ctx context.Context) error { return nil }
func (p *YourPlugin) Stop(ctx context.Context) error  { return nil }

// 必须导出：用于 PPM 读取元数据
var Metadata *plugin.PluginMetadata = &PluginMetadata
```

编译为 `.so`：
```bash
go build -buildmode=plugin -o your-plugin.so .
```

发布到 GitHub Releases 即可通过 PPM 安装。

## 仓库结构

```
PaperValet-Plugins/
├── plugins-external/          # 外部插件源码
│   ├── atadmins/
│   │   ├── main.go
│   │   └── go.mod
│   ├── calc/
│   │   ├── main.go
│   │   └── go.mod
│   └── ...
├── .github/workflows/         # CI: 自动编译 .so 并发布到 Releases
└── README.md
```

## CI/CD

GitHub Actions 自动：
1. 检测 `plugins-external/*/main.go` 变更
2. 编译 `go build -buildmode=plugin -o <name>.so`
3. 发布到 GitHub Releases `latest`
4. PPM 从 `https://github.com/TiaraBasori/PaperValet-Plugins/releases/latest/download/<name>.so` 下载

## 迁移自 TeleBox-Plugins

本仓库包含从 [TeleBoxOrg/TeleBox-Plugins](https://github.com/TeleBoxOrg/TeleBox-Plugins) 迁移的插件，重写为 Go + gotd 架构：

- 避免了 TypeScript/gramJS 的运行时依赖
- 编译为原生 `.so`，启动快、内存省
- 统一 PPM 包管理体验
- 类型安全的 SDK 接口

## 许可证

MIT License
