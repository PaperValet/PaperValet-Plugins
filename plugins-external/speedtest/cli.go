package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dataDir     = "data/speedtest"
	cliVersion  = "1.2.0"
	downloadURL = "https://install.speedtest.net/app/cli/"
	listTimeout = 30 * time.Second
	maxArchive  = 64 << 20

	// runTimeout bounds one speed test; the panel's timeout setting
	// (seconds) overrides it at run time.
	defaultRunTimeout = 120 * time.Second
	minRunTimeout     = 60 * time.Second
	maxRunTimeout     = 300 * time.Second
)

func exeName() string {
	if runtime.GOOS == "windows" {
		return "speedtest.exe"
	}
	return "speedtest"
}

// cliPath is the absolute path of the bundled Ookla CLI.
func cliPath() string {
	p := filepath.Join(dataDir, exeName())
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// cliCommand builds an exec.Cmd for a speedtest binary. The Ookla CLI
// aborts (std::logic_error) when $HOME is unset, which is the default
// under systemd, so HOME falls back to the plugin data directory.
func cliCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = cliEnv(os.Environ())
	return cmd
}

func cliEnv(env []string) []string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok && v != "" {
			return env
		}
	}
	home, err := filepath.Abs(dataDir)
	if err != nil {
		home = dataDir
	}
	return append(env, "HOME="+home)
}

// archiveName picks the Ookla package for the running OS/arch.
func archiveName(goos, goarch, goarm string) (string, error) {
	switch goos {
	case "linux":
		arch := map[string]string{
			"amd64": "x86_64", "arm64": "aarch64", "386": "i386",
		}[goarch]
		if goarch == "arm" {
			// GOARM isn't visible at run time; armhf covers ARMv7 boards,
			// pass goarm "5"/"6" for the soft-float armel build.
			arch = "armhf"
			if goarm == "5" || goarm == "6" {
				arch = "armel"
			}
		}
		if arch == "" {
			return "", fmt.Errorf("unsupported linux arch: %s", goarch)
		}
		return fmt.Sprintf("ookla-speedtest-%s-linux-%s.tgz", cliVersion, arch), nil
	case "darwin":
		return fmt.Sprintf("ookla-speedtest-%s-macosx-universal.tgz", cliVersion), nil
	case "windows":
		return fmt.Sprintf("ookla-speedtest-%s-win64.zip", cliVersion), nil
	case "freebsd":
		return "", errors.New("FreeBSD: please install speedtest via pkg and use --system")
	}
	return "", fmt.Errorf("unsupported platform: %s", goos)
}

// downloadCLI fetches and unpacks the Ookla CLI into data/speedtest.
// With force=false an existing binary is kept. dlMu serializes installs:
// list/test/best can trigger a download that would otherwise race a
// running test's install and corrupt the temp file.
var dlMu sync.Mutex

func downloadCLI(ctx context.Context, force bool) error {
	dlMu.Lock()
	defer dlMu.Unlock()
	path := cliPath()
	if !force {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	name, err := archiveName(runtime.GOOS, runtime.GOARCH, "")
	if err != nil {
		return err
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, downloadURL+name, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", name, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArchive))
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}

	tmp := path + ".new"
	defer os.Remove(tmp)
	var bin []byte
	if strings.HasSuffix(name, ".zip") {
		bin, err = extractZip(data, exeName())
	} else {
		bin, err = extractTgz(data, exeName())
	}
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

func extractTgz(data []byte, want string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == want {
			return io.ReadAll(io.LimitReader(tr, maxArchive))
		}
	}
	return nil, fmt.Errorf("%s not found in archive", want)
}

func extractZip(data []byte, want string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) == want {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, maxArchive))
		}
	}
	return nil, fmt.Errorf("%s not found in archive", want)
}

// removeCLI deletes the bundled binary and any leftover archives.
func removeCLI() {
	_ = os.Remove(cliPath())
	_ = os.Remove(cliPath() + ".new")
	entries, _ := os.ReadDir(dataDir)
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".tgz") || strings.HasSuffix(n, ".zip") || n == "speedtest.5" || n == "speedtest.md" {
			_ = os.Remove(filepath.Join(dataDir, n))
		}
	}
}

type diagnosis struct {
	canRun  bool
	exists  bool
	version string
	problem string
}

// diagnose checks that the bundled binary exists, is executable and runs.
func diagnose(ctx context.Context) diagnosis {
	path := cliPath()
	st, err := os.Stat(path)
	if err != nil {
		return diagnosis{problem: "可执行文件不存在 / executable missing"}
	}
	d := diagnosis{exists: true}
	if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
		if err := os.Chmod(path, 0o755); err != nil {
			d.problem = "权限修复失败 / chmod failed: " + err.Error()
			return d
		}
	}
	for _, arg := range []string{"--version", "--help"} {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, _ := cliCommand(c, path, arg).CombinedOutput()
		cancel()
		s := string(out)
		if strings.Contains(s, "Speedtest") || strings.Contains(strings.ToLower(s), "usage") {
			d.canRun = true
			if line, _, _ := strings.Cut(s, "\n"); strings.Contains(line, "Speedtest") {
				d.version = strings.TrimSpace(line)
			}
			return d
		}
	}
	d.problem = "可执行文件无法运行，可能是架构不匹配或文件损坏 / cannot run (arch mismatch or corrupt)"
	return d
}

// autoFix wipes and reinstalls the bundled CLI, then verifies it.
func autoFix(ctx context.Context) error {
	removeCLI()
	if err := downloadCLI(ctx, true); err != nil {
		return err
	}
	if d := diagnose(ctx); !d.canRun {
		return fmt.Errorf("自动修复失败 / auto-fix failed: %s", d.problem)
	}
	return nil
}

// ---------------------------------------------------------------- results

// Result is the normalized result of either CLI flavour.
type Result struct {
	ISP          string
	ServerID     int
	ServerName   string
	ServerLoc    string
	ServerCC     string
	ExternalIP   string
	Interface    string
	Latency      float64
	Jitter       float64
	PacketLoss   float64 // -1 when unknown
	DownBps      float64 // bytes/s
	UpBps        float64 // bytes/s
	DownBytes    float64
	UpBytes      float64
	UploadFailed bool
	Timestamp    string
	ResultURL    string // https://www.speedtest.net/result/c/<uuid>
	ImageURL     string
}

type ooklaResult struct {
	Type      string `json:"type"`
	Error     string `json:"error"`
	Timestamp string `json:"timestamp"`
	Ping      struct {
		Latency float64 `json:"latency"`
		Jitter  float64 `json:"jitter"`
	} `json:"ping"`
	Download *struct {
		Bandwidth float64 `json:"bandwidth"`
		Bytes     float64 `json:"bytes"`
	} `json:"download"`
	Upload *struct {
		Bandwidth *float64 `json:"bandwidth"`
		Bytes     float64  `json:"bytes"`
	} `json:"upload"`
	PacketLoss *float64 `json:"packetLoss"`
	ISP        string   `json:"isp"`
	Interface  struct {
		ExternalIP string `json:"externalIp"`
		Name       string `json:"name"`
	} `json:"interface"`
	Server struct {
		ID       int    `json:"id"`
		Name     string `json:"name"`
		Location string `json:"location"`
		Country  string `json:"country"`
	} `json:"server"`
	Result struct {
		URL string `json:"url"`
	} `json:"result"`
}

// parseOokla extracts the result object from the CLI's JSON (possibly
// multi-line) stdout.
func parseOokla(stdout string) (*Result, error) {
	var found *ooklaResult
	var lastErr string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var r ooklaResult
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if r.Error != "" {
			lastErr = r.Error
		}
		if r.Type == "result" || r.Download != nil {
			rr := r
			found = &rr
		}
	}
	if found == nil {
		if lastErr != "" {
			return nil, &runError{msg: lastErr, network: strings.Contains(lastErr, "Cannot read")}
		}
		return nil, errNotJSON
	}
	r := found
	res := &Result{
		ISP: r.ISP, ServerID: r.Server.ID, ServerName: r.Server.Name, ServerLoc: r.Server.Location,
		ExternalIP: r.Interface.ExternalIP, Interface: r.Interface.Name,
		Latency: r.Ping.Latency, Jitter: r.Ping.Jitter, PacketLoss: -1,
		Timestamp: r.Timestamp, ResultURL: r.Result.URL,
	}
	if r.Server.Country != "" {
		res.ServerCC = r.Server.Country
	}
	if r.PacketLoss != nil {
		res.PacketLoss = *r.PacketLoss
	}
	if r.Download != nil {
		res.DownBps, res.DownBytes = r.Download.Bandwidth, r.Download.Bytes
	}
	if r.Upload == nil || r.Upload.Bandwidth == nil {
		res.UploadFailed = true
	} else {
		res.UpBps, res.UpBytes = *r.Upload.Bandwidth, r.Upload.Bytes
	}
	if res.ResultURL != "" {
		res.ImageURL = res.ResultURL + ".png"
	}
	return res, nil
}

// pyResult is the JSON of the python "speedtest-cli" (sivel) tool.
type pyResult struct {
	Download  float64 `json:"download"` // bits/s
	Upload    float64 `json:"upload"`
	Ping      float64 `json:"ping"`
	Timestamp string  `json:"timestamp"`
	BytesSent float64 `json:"bytes_sent"`
	BytesRecv float64 `json:"bytes_received"`
	Share     *string `json:"share"`
	Server    struct {
		ID      json.RawMessage `json:"id"`
		Name    string          `json:"name"`
		Country string          `json:"country"`
		Sponsor string          `json:"sponsor"`
	} `json:"server"`
	Client struct {
		IP  string `json:"ip"`
		ISP string `json:"isp"`
	} `json:"client"`
}

func parsePy(stdout string) (*Result, error) {
	var r pyResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &r); err != nil {
		return nil, errNotJSON
	}
	id, _ := strconv.Atoi(strings.Trim(string(r.Server.ID), `"`))
	res := &Result{
		ISP: r.Client.ISP, ServerID: id, ServerName: r.Server.Sponsor,
		ServerLoc:  strings.Trim(r.Server.Name+", "+r.Server.Country, ", "),
		ExternalIP: r.Client.IP, Latency: r.Ping, PacketLoss: -1,
		DownBps: r.Download / 8, UpBps: r.Upload / 8,
		DownBytes: r.BytesRecv, UpBytes: r.BytesSent,
		Timestamp: r.Timestamp, UploadFailed: r.Upload == 0,
	}
	if r.Share != nil && *r.Share != "" {
		res.ImageURL = strings.Replace(*r.Share, "http://", "https://", 1)
		res.ResultURL = strings.TrimSuffix(res.ImageURL, ".png")
	}
	return res, nil
}

// ---------------------------------------------------------------- runner

var errNotJSON = errors.New("speedtest 返回非 JSON 输出 / non-JSON output")

type runError struct {
	msg     string
	network bool // environment problem, don't try to reinstall
	exec    bool // the binary itself failed to start
	noSrv   bool // server unavailable
}

func (e *runError) Error() string { return e.msg }

// classify turns CLI stderr / exec errors into a runError.
func classify(stderr string, err error) *runError {
	s := stderr
	// Ookla prints JSON log lines to stderr with -f json.
	var msgs []string
	for _, line := range strings.Split(stderr, "\n") {
		var l struct {
			Type, Message, Level string
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &l) == nil && l.Message != "" {
			msgs = append(msgs, l.Message)
		}
	}
	if len(msgs) > 0 {
		s = strings.Join(msgs, "; ")
	}
	s = strings.TrimSpace(s)
	switch {
	case strings.Contains(s, "NoServersException") || strings.Contains(s, "Server not found") || strings.Contains(s, "No servers"):
		return &runError{msg: "指定的服务器不可用 / server unavailable: " + s, noSrv: true}
	case strings.Contains(s, "Timeout occurred") || errors.Is(err, context.DeadlineExceeded):
		return &runError{msg: "网络连接超时，请检查网络状况或稍后重试 / timed out", network: true}
	case strings.Contains(s, "Cannot read"):
		return &runError{msg: "网络连接中断，可能是网络不稳定或防火墙阻止 / connection interrupted: " + s, network: true}
	case strings.Contains(s, "Upload: FAILED"):
		return &runError{msg: "上传测试失败 / upload test failed", network: true}
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		// exec format error, permission denied, ENOENT...
		return &runError{msg: "无法执行 speedtest / cannot exec: " + err.Error(), exec: true}
	}
	if s == "" && err != nil {
		s = err.Error()
	}
	return &runError{msg: s}
}

type flavour int

const (
	flavourOokla flavour = iota
	flavourPython
)

func runCLI(ctx context.Context, bin string, fl flavour, serverID int, timeout time.Duration) (*Result, error) {
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	var args []string
	if fl == flavourPython {
		args = []string{"--json", "--share", "--secure"}
		if serverID > 0 {
			args = append(args, "--server", strconv.Itoa(serverID))
		}
	} else {
		args = []string{"--accept-license", "--accept-gdpr", "-f", "json", "-p", "no"}
		if serverID > 0 {
			args = append(args, "-s", strconv.Itoa(serverID))
		}
	}
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := cliCommand(c, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if c.Err() == context.DeadlineExceeded {
		return nil, &runError{msg: "测试超时，可能网络较慢或服务器繁忙 / test timed out", network: true}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var res *Result
	var perr error
	if fl == flavourPython {
		res, perr = parsePy(stdout.String())
	} else {
		res, perr = parseOokla(stdout.String())
	}
	if perr == nil {
		return res, nil
	}
	var re *runError
	if errors.As(perr, &re) {
		return nil, re
	}
	if err != nil || stderr.Len() > 0 {
		return nil, classify(stderr.String(), err)
	}
	return nil, perr
}

// findSystemCLI looks up an installed speedtest binary and its flavour.
func findSystemCLI(ctx context.Context) (string, flavour, error) {
	for _, name := range []string{"speedtest", "speedtest-cli"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		// Skip our own bundled copy if it happens to be on PATH.
		if abs, _ := filepath.Abs(p); abs == cliPath() {
			continue
		}
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, _ := cliCommand(c, p, "--version").CombinedOutput()
		cancel()
		if strings.Contains(string(out), "Ookla") {
			return p, flavourOokla, nil
		}
		if strings.Contains(strings.ToLower(string(out)), "speedtest-cli") || strings.Contains(string(out), "Python") {
			return p, flavourPython, nil
		}
		return p, flavourOokla, nil
	}
	return "", 0, errors.New("系统未安装 speedtest / no system speedtest installed")
}

// runSpeedtest runs a test. useSystem prefers an installed binary and
// falls back to the bundled one; the bundled path installs/repairs itself
// once and falls back to auto server selection when the server is gone.
// timeout bounds one CLI run (panel setting).
func runSpeedtest(ctx context.Context, serverID int, useSystem bool, timeout time.Duration) (*Result, string, error) {
	if useSystem {
		bin, fl, err := findSystemCLI(ctx)
		if err == nil {
			res, err := runCLI(ctx, bin, fl, serverID, timeout)
			if err == nil {
				return res, "system", nil
			}
			var re *runError
			if !errors.As(err, &re) || re.network || ctx.Err() != nil {
				return nil, "system", err
			}
		}
		// fall back to the bundled CLI
	}

	if err := downloadCLI(ctx, false); err != nil {
		return nil, "", fmt.Errorf("下载 Speedtest CLI 失败 / download failed: %w", err)
	}
	if d := diagnose(ctx); !d.canRun {
		if err := autoFix(ctx); err != nil {
			return nil, "", err
		}
	}
	fixed := false
	for {
		res, err := runCLI(ctx, cliPath(), flavourOokla, serverID, timeout)
		if err == nil {
			return res, "bundled", nil
		}
		var re *runError
		if !errors.As(err, &re) {
			return nil, "bundled", err
		}
		switch {
		case re.noSrv && serverID > 0:
			serverID = 0 // auto selection
			continue
		case re.exec && !fixed:
			fixed = true
			if ferr := autoFix(ctx); ferr != nil {
				return nil, "bundled", fmt.Errorf("speedtest 可执行文件问题，自动修复失败 / auto-fix failed: %v", ferr)
			}
			continue
		}
		return nil, "bundled", err
	}
}

// ---------------------------------------------------------------- servers

type Server struct {
	ID       int    `json:"id"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Country  string `json:"country"`
}

func listServers(ctx context.Context) ([]Server, error) {
	if err := downloadCLI(ctx, false); err != nil {
		return nil, err
	}
	c, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	cmd := cliCommand(c, cliPath(), "--accept-license", "--accept-gdpr", "-f", "json", "-L")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var out struct {
		Servers []Server `json:"servers"`
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &out) == nil && len(out.Servers) > 0 {
			return out.Servers, nil
		}
	}
	if err != nil {
		return nil, classify(stderr.String(), err)
	}
	return nil, errors.New("无可用服务器 / no servers")
}
