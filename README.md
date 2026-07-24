# PaperValet-Plugins

PaperValet 外部插件仓库 - Go 插件包管理器 (PPM) 专用

## 插件列表

### 内建插件 (编译进二进制)
| 插件 | 命令 | 说明 |
|------|------|------|
| core | version/uptime/ping | 核心功能 |
| ppm | ppm | 插件包管理器 |
| help | help | 帮助系统 |
| admin | restart/shutdown/gc | 管理命令 |
| prefix | prefix | 多前缀管理 |
| cron | cron | 定时任务 |
| kitt | kitt | 高级触发器 (匹配→执行) |
| health | health/memory | health/memory | 内存守护 (自动 GC/重载/重启) |
| autofix | autofix | 一键修复 (git 同步+重启) |
| loglevel | loglevel | 运行时日志级别调整 |
| sendlog | sendlog | 发送日志到收藏夹 |
| save | save | 突破限制保存/转发消息 |
| leech | leech | 消息归档抓取 |
| ids | ids | 显示 ID 及跳转链接 |
| encode | encode | 编码/解码工具 |
| qr | qr | 二维码生成 |
| gt | gt | 谷歌翻译 |
| bizhi | bizhi | 随机壁纸 |
| weather | weather | 天气查询 |
| calc | calc | 计算器 |
| hitokoto | hitokoto | 随机一言 |
| rev | rev | 文本反转 |
| sendat | sendat | 定时发送 |
| isalive | isalive | 存活检测 |
| atadmins | atadmins | 艾特全体管理员 |

### 外部插件 (动态加载 .so)

| 插件 | 命令 | 说明 | PPM 安装 |
|------|------|------|----------|
| atadmins | atadmins | 一键艾特全部管理员 | `ppm install atadmins` |
| ids | ids | 显示用户/群组/消息 ID | `ppm install ids` |
| isalive | isalive | 检测 bot 运行状态 | `ppm install isalive` |
| calc | calc | 计算器 | `ppm install calc` |
| encode | encode | 编码/解码 (base64/url/hex) | `ppm install encode` |
| hitokoto | hitokoto | 随机一言 | `ppm install hitokoto` |
| qr | qr | 二维码生成 | `ppm install qr` |
| rev | rev | 文本反转 | `ppm install rev` |
| sendat | sendat | 定时消息发送 | `ppm install sendat` |
| gt | gt | 谷歌翻译 | `ppm install gt` |
| bizhi | bizhi | 随机壁纸 | `ppm install bizhi` |
| weather | weather | 天气查询 | `ppm install weather` |
| ping | ping | 网络延迟测试 (TCP/HTTP/ICMP/DC) | `ppm install ping` |
| leech | leech | 媒体下载 (yt-dlp) | `ppm install leech` |
| qrcode | qrcode | 二维码生成/解码 (完整版) | `ppm install qrcode` |
| re | re | 消息复读机 | `ppm install re` |
| sendlog | sendlog | 日志发送工具 | `ppm install sendlog` |
| tpm | tpm | 旧版插件管理器 | `ppm install tpm` |
| bf | bf | 备份工具 | `ppm install bf` |

## 使用方法

### 安装插件
```bash
ppm install atadmins ids calc
```

### 加载插件
```bash
ppm load atadmins
```

### 查看已安装
```bash
ppm list
```

### 搜索插件
```bash
ppm search 翻译
```

### 卸载插件
```bash
ppm uninstall gt
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