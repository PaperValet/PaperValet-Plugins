package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"image"
	"image/color"
	"os/exec"
	"strings"
	"testing"
)

const ooklaSample = `{"type":"result","timestamp":"2024-05-01T10:20:30Z","ping":{"jitter":0.512,"latency":3.25,"low":3.1,"high":4},"download":{"bandwidth":117000000,"bytes":1200000000,"elapsed":10000},"upload":{"bandwidth":58500000,"bytes":600000000,"elapsed":10000},"packetLoss":0.5,"isp":"Hetzner Online","interface":{"internalIp":"10.0.0.2","name":"eth0","macAddr":"00:00:00:00:00:00","isVpn":false,"externalIp":"1.2.3.4"},"server":{"id":28922,"host":"speednld.phoenixnap.com","port":8080,"name":"PhoenixNAP","location":"Amsterdam","country":"Netherlands","ip":"1.1.1.1"},"result":{"id":"abc","url":"https://www.speedtest.net/result/c/abc","persisted":true}}`

func TestParseOokla(t *testing.T) {
	r, err := parseOokla(ooklaSample)
	if err != nil {
		t.Fatal(err)
	}
	if r.ISP != "Hetzner Online" || r.ServerID != 28922 || r.Latency != 3.25 || r.Jitter != 0.512 ||
		r.PacketLoss != 0.5 || r.UploadFailed || r.ImageURL != "https://www.speedtest.net/result/c/abc.png" {
		t.Fatalf("%+v", r)
	}
	// no upload, no packet loss
	r, err = parseOokla(`{"type":"result","download":{"bandwidth":1000,"bytes":10},"ping":{"latency":1}}`)
	if err != nil || !r.UploadFailed || r.PacketLoss != -1 {
		t.Fatalf("%+v %v", r, err)
	}
	_, err = parseOokla(`{"error":"Cannot read from socket"}`)
	var re *runError
	if !errors.As(err, &re) || !re.network {
		t.Fatalf("%v", err)
	}
	if _, err := parseOokla("garbage"); err != errNotJSON {
		t.Fatal(err)
	}
}

func TestParsePy(t *testing.T) {
	s := `{"download": 93847562.1, "upload": 45000000.0, "ping": 12.3, "server": {"url": "x", "name": "Amsterdam", "country": "Netherlands", "id": "1234", "sponsor": "Foo"}, "timestamp": "2024-05-01T10:20:30.123456Z", "bytes_sent": 1000, "bytes_received": 2000, "share": "http://www.speedtest.net/result/123.png", "client": {"ip": "1.2.3.4", "isp": "ISP"}}`
	r, err := parsePy(s)
	if err != nil {
		t.Fatal(err)
	}
	if r.ServerID != 1234 || r.ImageURL != "https://www.speedtest.net/result/123.png" || r.ResultURL != "https://www.speedtest.net/result/123" || r.DownBps*8 != 93847562.1 {
		t.Fatalf("%+v", r)
	}
}

func TestUnitConvert(t *testing.T) {
	if got := unitConvert(117000000, false); got != "936Mbps" {
		t.Error(got)
	}
	if got := unitConvert(1234567, true); got != "1.23MB" {
		t.Error(got)
	}
	if got := unitConvert(0, false); got != "0bps" {
		t.Error(got)
	}
}

func TestArchiveName(t *testing.T) {
	cases := map[[2]string]string{
		{"linux", "amd64"}:   "ookla-speedtest-1.2.0-linux-x86_64.tgz",
		{"linux", "arm64"}:   "ookla-speedtest-1.2.0-linux-aarch64.tgz",
		{"linux", "arm"}:     "ookla-speedtest-1.2.0-linux-armhf.tgz",
		{"linux", "386"}:     "ookla-speedtest-1.2.0-linux-i386.tgz",
		{"darwin", "arm64"}:  "ookla-speedtest-1.2.0-macosx-universal.tgz",
		{"windows", "amd64"}: "ookla-speedtest-1.2.0-win64.zip",
	}
	for in, want := range cases {
		got, err := archiveName(in[0], in[1], "")
		if err != nil || got != want {
			t.Errorf("%v: %s %v", in, got, err)
		}
	}
	if _, err := archiveName("linux", "mips", ""); err == nil {
		t.Error("mips accepted")
	}
}

func TestMisc(t *testing.T) {
	if flagEmoji("nl") != "🇳🇱" || flagEmoji("x") != "" {
		t.Error("flag")
	}
	if formatTimestamp("2024-05-01T10:20:30Z") != "2024-05-01 10:20:30" ||
		formatTimestamp("2024-05-01T10:20:30.123456Z") != "2024-05-01 10:20:30" {
		t.Error("ts")
	}
	if o := orderString(messageOrder("file")); o != "file → photo → sticker → txt" {
		t.Error(o)
	}
	if c := classify(`{"type":"log","message":"Configuration - No servers defined (NoServersException)","level":"error"}`, errors.New("exit")); !c.noSrv {
		t.Errorf("%+v", c)
	}
	if c := classify("", &exec.Error{Name: "x", Err: exec.ErrNotFound}); !c.exec {
		t.Errorf("%+v", c)
	}
}

func TestFormatResult(t *testing.T) {
	r, _ := parseOokla(ooklaSample)
	out := formatResult("zh-CN", r, extraInfo{ip: ipInfo{asInfo: "AS24940", ccCode: "DE", ccFlag: "🇩🇪"}, mtu: 1500, rx: 1e9, tx: 2e9, haveTraffic: true})
	for _, want := range []string{"Hetzner Online", "28922", "MTU", "⇔3.25ms", "±0.51ms", "Loss", "↓936Mbps", "↑468Mbps", "RX 1GB", "2024-05-01 10:20:30", "speedtest.net/result/c/abc", "@DE🇩🇪"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestExtractTgz(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{{"speedtest.md", "doc"}, {"speedtest", "BIN"}} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(f.body))
	}
	tw.Close()
	gz.Close()
	b, err := extractTgz(buf.Bytes(), "speedtest")
	if err != nil || string(b) != "BIN" {
		t.Fatal(string(b), err)
	}
}

func TestFillCorners(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 40, 30)) // fully transparent
	src.Set(20, 15, color.White)
	out := fillCorners(src, color.RGBA{0x21, 0x23, 0x38, 0xff}, 14)
	if r, g, b, a := out.At(0, 0).RGBA(); r>>8 != 0x21 || g>>8 != 0x23 || b>>8 != 0x38 || a>>8 != 0xff {
		t.Error("corner not filled")
	}
	if r, _, _, _ := out.At(20, 15).RGBA(); r>>8 != 0xff {
		t.Error("centre lost")
	}
}
