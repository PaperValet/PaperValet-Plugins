# PaperValet-Plugins

External plugins for [PaperValet](https://github.com/TiaraBasori/PaperValet).

## Available Plugins

| Plugin | Description | Status |
|--------|-------------|--------|
| **ping** | 网络延迟测试工具 (TCP/HTTP/ICMP/DC) | ✅ |
| **leech** | 媒体下载工具 (yt-dlp) | ✅ |
| **qrcode** | 二维码生成与解码 | ✅ |
| **bf** | Brainfuck 解释器 | ✅ |
| **re** | 消息复读机 | ✅ |
| **sendlog** | 日志发送工具 | ✅ |
| **tpm** | Telegram 插件管理器 (旧版) | ✅ |

## Usage

```bash
# Install via PaperValet PPM
ppm install ping
ppm load ping

# Or build manually
git clone https://github.com/TiaraBasori/PaperValet-Plugins
cd plugins-external/ping
go build -buildmode=plugin -o ping.so .
cp ping.so /path/to/papervalet/plugins/
```

## Release

Tag with `plugins/<name>/<version>` to trigger CI build:

```bash
git tag plugins/ping/v1.0.0
git push origin plugins/ping/v1.0.0
```

Or use `workflow_dispatch` from GitHub Actions UI to build specific plugins.

## License

MIT