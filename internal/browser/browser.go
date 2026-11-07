// Package browser 负责「服务端内置浏览器」的生命周期：
// 探测本机 Chromium 内核浏览器（Edge / Chrome）、以独立用户目录启动、
// 通过 CDP 连接并开出可被脚本驱动的标签页。
package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"uitest-server/internal/cdp"
	"uitest-server/internal/model"
)

// Status 内置浏览器状态（给控制台看）。
type Status struct {
	Running     bool     `json:"running"`
	Executable  string   `json:"executable"`
	DebugURL    string   `json:"debug_url"`
	ProfileDir  string   `json:"profile_dir"`
	Version     string   `json:"version"`
	Headless    bool     `json:"headless"`
	LogFile     string   `json:"log_file"`
	Attached    bool     `json:"attached"`
	DebugURLSet string   `json:"debug_url_set"`
	KernelDir   string   `json:"kernel_dir"`
	LaunchFlags []string `json:"launch_flags"`
	RendererOK  bool     `json:"renderer_ok"`
	Probe       Probe    `json:"probe"`
}

// Manager 浏览器进程与 CDP 连接的管理者。
type Manager struct {
	mu         sync.Mutex
	exe        string
	headless   bool
	noSandbox  bool
	external   string // 非空=连接已有浏览器
	attached   bool   // true=外部实例（Close 只断开连接，不杀进程）
	profileDir string
	kernelDir  string   // 自带内核目录（默认 <data>/../browser）
	extra      []string // 额外启动参数
	resolved   []string // 探测成功过的启动参数组合（记住它，下次直接用）
	// attemptProfile 本次尝试使用的 user-data-dir（重试时换独立目录，避开 profile 占用）
	attemptProfile string
	// baseProfile 去掉重试后缀的基准 profile 目录（否则每次重启名字都会再叠一层）
	baseProfile string
	launchFlags []string
	rendererOK  bool
	lastProbe   Probe
	onResolve   func(flags []string, profileDir string) // 探测成功后回调：持久化可用参数与 profile 目录
	port        int
	cmd         *exec.Cmd
	client      *cdp.Client
	version     string
	debugURL    string
	logFile     string

	// 启动/关闭期间会长时间持有 mu，Status() 用这份快照兜底，避免控制台接口被拖死
	cacheMu      sync.Mutex
	cachedStatus Status
}

// NewManager 创建管理器（不会立刻启动浏览器）。
func NewManager(cfg model.BrowserSettings, profileDir, logDir string) *Manager {
	return &Manager{
		exe:         cfg.Executable,
		headless:    cfg.Headless,
		noSandbox:   cfg.NoSandbox,
		external:    strings.TrimRight(cfg.DebugURL, "/"),
		profileDir:  firstNonEmpty(cfg.ProfileDir, profileDir),
		baseProfile: stripRetrySuffix(firstNonEmpty(cfg.ProfileDir, profileDir)),
		kernelDir:   cfg.KernelDir,
		extra:       append([]string(nil), cfg.ExtraArgs...),
		resolved:    append([]string(nil), cfg.ResolvedFlags...),
		logFile:     filepath.Join(logDir, "browser.log"),
		port:        9222,
	}
}

// ApplySettings 更新配置（浏览器未运行时才生效）。
func (m *Manager) ApplySettings(cfg model.BrowserSettings) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exe = cfg.Executable
	m.kernelDir = cfg.KernelDir
	m.extra = append([]string(nil), cfg.ExtraArgs...)
	if len(cfg.ResolvedFlags) > 0 {
		m.resolved = append([]string(nil), cfg.ResolvedFlags...)
	}
	if m.cmd == nil && !m.attached {
		m.headless = cfg.Headless
		m.noSandbox = cfg.NoSandbox
	}
	next := strings.TrimRight(cfg.DebugURL, "/")
	if next != m.external {
		// 外部端点变了：断开旧连接，下次 Ensure 重新连
		if m.client != nil {
			_ = m.client.Close()
			m.client = nil
		}
		m.attached = false
		m.external = next
	}
}

// Status 当前状态。
//
// 注意：浏览器启动/关闭期间会长时间持有 m.mu（要等进程、等 CDP、做探测），
// 这里用 TryLock + 快照，保证控制台的 /api/health、/api/browser/status 不会跟着卡住。
func (m *Manager) Status() Status {
	if !m.mu.TryLock() {
		m.cacheMu.Lock()
		st := m.cachedStatus
		m.cacheMu.Unlock()
		return st
	}
	defer m.mu.Unlock()
	st := m.snapshotLocked()
	m.cacheMu.Lock()
	m.cachedStatus = st
	m.cacheMu.Unlock()
	return st
}

// snapshotLocked 生成状态快照（调用方需持有 m.mu）。
func (m *Manager) snapshotLocked() Status {
	st := Status{
		Running:     m.client != nil,
		Executable:  m.exe,
		DebugURL:    m.debugURL,
		ProfileDir:  m.profileDir,
		Version:     m.version,
		Headless:    m.headless,
		LogFile:     m.logFile,
		Attached:    m.attached,
		DebugURLSet: m.external,
		KernelDir:   m.kernelDir,
		LaunchFlags: append([]string(nil), m.launchFlags...),
		RendererOK:  m.rendererOK,
		Probe:       m.lastProbe,
	}
	if st.Executable == "" {
		if p, err := FindExecutable(""); err == nil {
			st.Executable = p
		}
	}
	if m.attached {
		st.Executable = "（外部浏览器）" + st.Executable
	}
	return st
}

// recordSnapshotLocked 把当前状态写进快照缓存（调用方需持有 m.mu）。
func (m *Manager) recordSnapshotLocked() {
	st := m.snapshotLocked()
	m.cacheMu.Lock()
	m.cachedStatus = st
	m.cacheMu.Unlock()
}

// Ensure 确保浏览器已启动并连上 CDP。
//
// 启动带自愈：按 flagCandidates() 逐个尝试启动参数组合，每组启动后都用
// ProbeRenderer 做一次真实页面探测（能拿到 document.title 才算渲染进程活着）。
// 命中可用组合后通过 onResolve 持久化，下次直接用它，不再反复试。
// 这样"浏览器窗口打得开、但页面全是白的/打不开任何网站"的环境可以自动恢复。
func (m *Manager) Ensure(ctx context.Context) (*cdp.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != nil {
		select {
		case <-m.client.Closed():
			m.client = nil
			m.cmd = nil
			m.rendererOK = false
		default:
			return m.client, nil
		}
	}
	if m.external != "" {
		client, version, err := attachExisting(ctx, m.external)
		if err != nil {
			return nil, fmt.Errorf("连接已有浏览器失败（%s）: %w", m.external, err)
		}
		m.client = client
		m.attached = true
		m.debugURL = m.external
		m.version = version
		if port, perr := parsePort(m.external); perr == nil {
			m.port = port
		}
		probe := probeWith(ctx, client, m.port, "about:blank")
		m.rendererOK = probe.OK
		m.lastProbe = probe
		if !probe.OK {
			return nil, fmt.Errorf("已连上 %s，但页面无法渲染：%s", m.external, probe.Error)
		}
		return client, nil
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil, errors.New("不支持的操作系统")
	}

	// 先看有没有上一次运行遗留、进程还活着的实例可以接管（否则 profile 被占用会让启动直接失败）
	if client, port, version, aerr := adoptProfile(ctx, m.profileDir); aerr == nil {
		m.client, m.port, m.version = client, port, version
		m.debugURL = fmt.Sprintf("http://127.0.0.1:%d", port)
		probe := probeWith(ctx, client, port, "about:blank")
		if probe.OK {
			m.rendererOK = true
			m.lastProbe = probe
			m.recordSnapshotLocked()
			log.Printf("[browser] 复用了已在运行的浏览器实例（调试端口 %d）", port)
			return client, nil
		}
		log.Printf("[browser] 遗留实例的渲染进程不可用，先关掉它：%s", probe.Error)
		_ = client.Call(context.Background(), "", "Browser.close", nil, nil)
		_ = client.Close()
		m.client = nil
		time.Sleep(1800 * time.Millisecond)
	}

	// 再找一遍"数据目录里还活着的兄弟实例"（强杀服务端后常见孤儿进程，接管比重启快）
	if client, port, version, dir, aerr := adoptAnySibling(ctx, m.baseProfile); aerr == nil {
		m.client, m.port, m.version = client, port, version
		m.profileDir = dir
		m.debugURL = fmt.Sprintf("http://127.0.0.1:%d", port)
		m.rendererOK = true
		m.recordSnapshotLocked()
		log.Printf("[browser] 接管了已在运行的浏览器实例（profile=%s，端口 %d）", dir, port)
		return client, nil
	}

	exe, err := m.resolveExeLocked()
	if err != nil {
		return nil, err
	}

	// profile 里可能残留上次被强杀留下的单实例锁：不清掉的话新进程会"委托给已有会话"直接退出
	clearStaleProfileLocks(m.profileDir)
	m.cleanStaleRetryProfiles()
	cands := m.flagCandidates()
	var lastErr error
	for i, flags := range cands {
		if ctx.Err() != nil {
			lastErr = errors.New("启动超时（环境过慢或浏览器被拦截）")
			break
		}
		// 第一次用正式 profile，后续重试换「唯一」目录：
		// Chromium 的 profile 里有 SingletonLock，复用同名目录时残留锁会让新进程
		// "委托给已有会话"然后直接退出（表现就是调试端口永远等不到）。
		if i == 0 {
			m.attemptProfile = ""
		} else {
			m.attemptProfile = fmt.Sprintf("%s-retry%d-%d", m.baseProfile, i, time.Now().UnixNano()%1e6)
		}
		log.Printf("[browser] 启动尝试 %d/%d，附加参数：%v", i+1, len(cands), flags)
		if err := m.launchOnce(ctx, exe, flags); err != nil {
			lastErr = err
			log.Printf("[browser] 启动失败：%v", err)
			continue
		}
		probe := probeWith(ctx, m.client, m.port, "about:blank")
		if probe.OK {
			m.rendererOK = true
			m.lastProbe = probe
			// 成功的组合与它用的 profile 目录一起固化：下次可以直接接管同一实例，
			// 登录态也能保留（重试目录只在这一轮用，成功后就成为正式目录）
			if m.attemptProfile != "" {
				m.profileDir = m.attemptProfile
				m.attemptProfile = ""
			}
			if m.onResolve != nil {
				m.onResolve(append([]string(nil), flags...), m.profileDir)
			}
			return m.client, nil
		}
		lastErr = fmt.Errorf("浏览器已启动但页面打不开（%s）。启动参数：%v",
			probe.Error, strings.Join(flags, " "))
		log.Printf("[browser] 该组合不可用：%s", probe.Error)
		m.rendererOK = false
		m.lastProbe = probe
		m.killLocked()
	}
	if lastErr == nil {
		lastErr = errors.New("浏览器启动失败")
	}
	return nil, fmt.Errorf("%w；可查看 data/logs/browser.log，或在设置里改用自带内核 / 连接已有浏览器", lastErr)
}

// Close 关闭浏览器进程（接管已有浏览器时只断开 CDP，不杀进程）。
func (m *Manager) Close() error {
	m.mu.Lock()
	client := m.client
	cmd := m.cmd
	m.client = nil
	m.cmd = nil
	m.mu.Unlock()

	var firstErr error
	wasAttached := m.attached
	if client != nil {
		if wasAttached {
			_ = client.Close()
		} else {
			ctx, cancel := cdp.WithTimeout(context.Background(), 3*time.Second)
			_ = client.Call(ctx, "", "Browser.close", nil, nil)
			cancel()
			_ = client.Close()
		}
	}
	if wasAttached && cmd == nil {
		return firstErr
	}
	if cmd != nil && cmd.Process != nil {
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			if err := cmd.Process.Kill(); err != nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// FindExecutable 探测本机 Chromium 内核浏览器路径。
func FindExecutable(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", fmt.Errorf("指定的浏览器路径不存在: %s", explicit)
	}
	for _, env := range []string{"UITEST_BROWSER", "CHROME_PATH", "EDGE_PATH"} {
		if v := os.Getenv(env); v != "" {
			if _, err := os.Stat(v); err == nil {
				return v, nil
			}
		}
	}
	candidates := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		candidates = append(candidates,
			filepath.Join(la, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(la, `Microsoft\Edge\Application\msedge.exe`),
		)
	}
	candidates = append(candidates,
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		"/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/microsoft-edge",
	)
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	for _, name := range []string{"msedge", "chrome", "chromium", "google-chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("未找到 Edge / Chrome，请在「设置」里填写浏览器可执行文件路径")
}

func freePort(start int) (int, error) {
	for p := start; p < start+40; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			addr := ln.Addr().(*net.TCPAddr)
			_ = ln.Close()
			return addr.Port, nil
		}
	}
	return 0, errors.New("没有可用端口启动调试浏览器")
}

// waitForDebugger 轮询 /json/version 直到浏览器就绪，返回 WebSocket 地址与版本号。
func waitForDebugger(ctx context.Context, port int, timeout time.Duration) (string, string, error) {
	return probeDebugger(ctx, fmt.Sprintf("http://127.0.0.1:%d", port), timeout)
}

// attachExisting 连接一个已在运行的 CDP 端点（外部浏览器）。
func attachExisting(ctx context.Context, debugURL string) (*cdp.Client, string, error) {
	wsURL, version, err := probeDebugger(ctx, debugURL, 10*time.Second)
	if err != nil {
		return nil, "", err
	}
	client, err := cdp.Dial(ctx, wsURL)
	if err != nil {
		return nil, "", fmt.Errorf("拨号 %s 失败: %w", wsURL, err)
	}
	return client, version, nil
}

// parsePort 从 http://127.0.0.1:9400 这类地址里取出端口。
func parsePort(debugURL string) (int, error) {
	idx := strings.LastIndex(debugURL, ":")
	if idx < 0 {
		return 0, errors.New("地址里没有端口")
	}
	portStr := strings.TrimRight(debugURL[idx+1:], "/")
	port := 0
	for _, c := range portStr {
		if c < '0' || c > '9' {
			return 0, errors.New("端口不是数字")
		}
		port = port*10 + int(c-'0')
	}
	if port == 0 {
		return 0, errors.New("端口无效")
	}
	return port, nil
}

// probeDebugger 轮询 <base>/json/version 直到就绪。
func probeDebugger(ctx context.Context, base string, timeout time.Duration) (string, string, error) {
	deadline := time.Now().Add(timeout)
	url := strings.TrimRight(base, "/") + "/json/version"
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var v struct {
				Browser              string `json:"Browser"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			if json.Unmarshal(raw, &v) == nil && v.WebSocketDebuggerURL != "" {
				return v.WebSocketDebuggerURL, strings.TrimPrefix(v.Browser, "Headless"), nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", "", fmt.Errorf("等待浏览器调试端口超时（%s）：浏览器可能被安全软件阻拦，"+
		"或它把启动请求转交给了已在运行的实例（日志里会出现「正在现有浏览器会话中打开」）", timeout)
}

/* ------------------------------------------------------------------ Page */

// Page 一个可被脚本驱动的标签页。
//
// 传输方式说明：新版 Chromium（Edge 130+ 实测）在「浏览器级 WebSocket +
// 扁平化会话（flatten）」上不返回会话级命令的响应，因此这里优先直连
// 页面级端点 ws://127.0.0.1:<port>/devtools/page/<targetId>；只有该端点
// 不可用时才回退到扁平化会话。
type Page struct {
	client    *cdp.Client // 承载页面命令的连接（页面级，或浏览器级+会话）
	parent    *cdp.Client // 浏览器级连接，用于建/关标签页
	SessionID string      // 空表示页面级连接
	TargetID  string
	URL       string
}

// call 统一下发页面命令。
func (p *Page) call(ctx context.Context, method string, params any, result any) error {
	return p.client.Call(ctx, p.SessionID, method, params, result)
}

// OpenPage 新建标签页并建立可用的页面连接。
func (m *Manager) OpenPage(ctx context.Context, url string) (*Page, error) {
	client, err := m.Ensure(ctx)
	if err != nil {
		return nil, err
	}
	if url == "" {
		url = "about:blank"
	}
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := client.Call(ctx, "", "Target.createTarget", map[string]any{"url": url, "background": false}, &created); err != nil {
		return nil, fmt.Errorf("新建标签页失败: %w", err)
	}

	m.mu.Lock()
	port := m.port
	m.mu.Unlock()

	pageWS := fmt.Sprintf("ws://127.0.0.1:%d/devtools/page/%s", port, created.TargetID)
	pageClient, dialErr := cdp.Dial(ctx, pageWS)

	p := &Page{parent: client, TargetID: created.TargetID, URL: url}
	if dialErr == nil {
		p.client = pageClient
	} else {
		// 回退：浏览器级连接 + 扁平化会话
		var attached struct {
			SessionID string `json:"sessionId"`
		}
		if err := client.Call(ctx, "", "Target.attachToTarget", map[string]any{
			"targetId": created.TargetID, "flatten": true,
		}, &attached); err != nil {
			_ = client.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": created.TargetID}, nil)
			return nil, fmt.Errorf("连接标签页失败（页面端点: %v / 会话端点: %v）", dialErr, err)
		}
		p.client = client
		p.SessionID = attached.SessionID
	}

	if err := p.call(ctx, "Page.enable", nil, nil); err != nil {
		p.Close(context.Background())
		return nil, fmt.Errorf("标签页不可用（%v）：内置浏览器渲染进程可能崩溃了，"+
			"可在 config.json 里把 browser.no_sandbox 设为 true 后重启服务", err)
	}
	for _, method := range []string{"Runtime.enable", "DOM.enable", "Network.enable"} {
		_ = p.call(ctx, method, nil, nil)
	}
	_ = client.Call(ctx, "", "Target.activateTarget", map[string]any{"targetId": created.TargetID}, nil)
	return p, nil
}

// Eval 在页面里求值，返回 JSON 值（要求结果可 JSON 序列化）。
func (p *Page) Eval(ctx context.Context, expr string) (json.RawMessage, error) {
	var res struct {
		Result struct {
			Type        string          `json:"type"`
			Value       json.RawMessage `json:"value"`
			Description string          `json:"description"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	params := map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
		"userGesture":   true,
	}
	if err := p.call(ctx, "Runtime.evaluate", params, &res); err != nil {
		return nil, err
	}
	if res.ExceptionDetails != nil {
		msg := res.ExceptionDetails.Text
		if res.ExceptionDetails.Exception.Description != "" {
			msg = res.ExceptionDetails.Exception.Description
		}
		return nil, errors.New("页面脚本异常: " + msg)
	}
	return res.Result.Value, nil
}

// EvalInto 求值并反序列化到 out。
func (p *Page) EvalInto(ctx context.Context, expr string, out any) error {
	raw, err := p.Eval(ctx, expr)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return errors.New("页面返回值为空")
	}
	return json.Unmarshal(raw, out)
}

// Navigate 导航到 url 并等待 document 进入 complete。
func (p *Page) Navigate(ctx context.Context, url string) error {
	loadCh, cancelLoad := p.client.Subscribe("Page.loadEventFired")
	defer cancelLoad()

	var nav struct {
		FrameID   string `json:"frameId"`
		ErrorText string `json:"errorText"`
	}
	if err := p.call(ctx, "Page.navigate", map[string]any{"url": url}, &nav); err != nil {
		return err
	}
	if nav.ErrorText != "" {
		return errors.New("导航失败: " + nav.ErrorText)
	}
	p.URL = url

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-loadCh:
			// 等到 readyState=complete，避免 load 事件早于 SPA 首屏渲染
			if p.waitReady(ctx, 5*time.Second) {
				return nil
			}
		case <-time.After(250 * time.Millisecond):
			if p.waitReady(ctx, 100*time.Millisecond) {
				return nil
			}
		}
	}
	return errors.New("导航超时：页面 20s 内未就绪")
}

func (p *Page) waitReady(ctx context.Context, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		raw, err := p.Eval(ctx, "document.readyState")
		if err == nil && string(raw) == `"complete"` {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(80 * time.Millisecond)
	}
}

// Screenshot 截图（PNG 字节）。
func (p *Page) Screenshot(ctx context.Context) ([]byte, error) {
	var res struct {
		Data string `json:"data"`
	}
	if err := p.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"}, &res); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(res.Data)
}

// ScreenshotClip 截取页面上的一块区域（CSS 像素，页面坐标系）。
// 验证码识别用它只把验证码图片本身发出去，避免整页截图泄露其它内容。
func (p *Page) ScreenshotClip(ctx context.Context, x, y, w, h float64) ([]byte, error) {
	if w <= 0 || h <= 0 {
		return nil, errors.New("截图区域无效")
	}
	var res struct {
		Data string `json:"data"`
	}
	params := map[string]any{
		"format": "png",
		"clip": map[string]any{
			"x": x, "y": y, "width": w, "height": h, "scale": 1,
		},
		"captureBeyondViewport": true,
	}
	if err := p.call(ctx, "Page.captureScreenshot", params, &res); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(res.Data)
}

// SetFileInput 给 file input 设置本地文件（css 为唯一选择器）。
func (p *Page) SetFileInput(ctx context.Context, css string, files []string) error {
	var doc struct {
		Root struct {
			NodeID int `json:"nodeId"`
		} `json:"root"`
	}
	if err := p.call(ctx, "DOM.getDocument", map[string]any{"depth": 0}, &doc); err != nil {
		return err
	}
	var found struct {
		NodeID int `json:"nodeId"`
	}
	if err := p.call(ctx, "DOM.querySelector", map[string]any{
		"nodeId": doc.Root.NodeID, "selector": css,
	}, &found); err != nil {
		return err
	}
	if found.NodeID == 0 {
		return errors.New("未找到文件选择框: " + css)
	}
	return p.call(ctx, "DOM.setFileInputFiles", map[string]any{
		"nodeId": found.NodeID, "files": files,
	}, nil)
}

// Close 关闭标签页并释放连接。
func (p *Page) Close(ctx context.Context) {
	if p == nil {
		return
	}
	if p.parent != nil {
		_ = p.parent.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": p.TargetID}, nil)
	}
	if p.client != nil && p.client != p.parent {
		_ = p.client.Close()
	}
}
