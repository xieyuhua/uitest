package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"uitest-server/internal/cdp"
)

// Probe 一次页面级健康检查的结果。
type Probe struct {
	OK         bool   `json:"ok"`
	Title      string `json:"title"`
	ReadyState string `json:"ready_state"`
	Error      string `json:"error"`
	DurationMS int64  `json:"duration_ms"`
}

// flagCandidates 返回"启动参数组合"的尝试顺序。
//
// 背景：某些受限环境（容器 / 受限 Job 对象 / 杀软拦截）里 Chromium 的渲染进程
// 会因为沙箱注册失败直接崩掉（CDP 报 Target crashed、日志里是
// AppContainerRegisterSid failed / Failed to get peer PID）。
// 实测在这种环境下追加 --no-sandbox 即可恢复，所以这里做自动降级尝试，
// 一旦某个组合探测成功就记住它（下次直接用它启动，不再反复试）。
func (m *Manager) flagCandidates() [][]string {
	cands := [][]string{}
	if len(m.resolved) > 0 {
		cands = append(cands, append([]string(nil), m.resolved...))
	}
	if m.noSandbox {
		cands = append(cands, []string{"--no-sandbox", "--disable-dev-shm-usage"})
	} else {
		cands = append(cands, nil)
	}
	cands = append(cands,
		[]string{"--no-sandbox", "--disable-dev-shm-usage"},
		[]string{"--no-sandbox", "--disable-dev-shm-usage", "--in-process-gpu", "--disable-gpu"},
		[]string{"--no-sandbox", "--disable-dev-shm-usage", "--disable-gpu", "--disable-software-rasterizer"},
	)
	// 去重，避免用户已经勾了 no_sandbox 时重复试同一组
	seen := map[string]bool{}
	out := make([][]string, 0, len(cands))
	for _, c := range cands {
		key := strings.Join(c, " ")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

// buildArgsLocked 组装完整命令行参数（调用方需持有 m.mu，切勿在此再加锁，否则自死锁）。
func (m *Manager) buildArgsLocked(port int, flags []string) []string {
	profileDir, headless := m.profileDir, m.headless
	if m.attemptProfile != "" {
		profileDir = m.attemptProfile
	}
	extra := append([]string(nil), m.extra...)

	args := []string{}
	if headless {
		args = append(args, "--headless=new", "--disable-gpu", "--window-size=1440,960")
	}
	args = append(args, flags...)
	args = append(args, extra...)
	args = append(args,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-allow-origins=*",
		"--user-data-dir="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-session-crashed-bubble",
		"--disable-popup-blocking",
		"--disable-features=Translate,msEdgeSidebarV2,msImplicitSignin,msEdgeCollections",
		"--start-maximized",
		"about:blank",
	)
	return args
}

// launchOnce 启动一次浏览器进程并连上 CDP（调用方需持有 m.mu）。
func (m *Manager) launchOnce(ctx context.Context, exe string, flags []string) error {
	if err := os.MkdirAll(m.attemptProfileOr(), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.logFile), 0o755); err != nil {
		return err
	}
	port, err := freePort(9222)
	if err != nil {
		return err
	}
	m.port = port

	args := m.buildArgsLocked(port, flags)
	logf, _ := os.OpenFile(m.logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if logf != nil {
		fmt.Fprintf(logf, "\n==== %s 启动：%s %s\n", time.Now().Format("2006-01-02 15:04:05"), exe, strings.Join(args, " "))
	}
	cmd := exec.Command(exe, args...)
	if logf != nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	if err := cmd.Start(); err != nil {
		if logf != nil {
			logf.Close()
		}
		return fmt.Errorf("启动浏览器失败: %w", err)
	}
	m.cmd = cmd
	m.debugURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	m.launchFlags = append([]string(nil), flags...)

	m.recordSnapshotLocked()

	// 快速识别「委托给已有会话」：Chromium 发现 profile 被占用时会打印
	// 「正在现有浏览器会话中打开 / Opening in existing browser session」并立刻退出，
	// 这时调试端口永远不会起来。与其干等 14s，不如马上判定失败、换下一组参数。
	logStart := int64(0)
	if st, err := os.Stat(m.logFile); err == nil {
		logStart = st.Size()
	}
	for i := 0; i < 10; i++ {
		if raw, err := os.ReadFile(m.logFile); err == nil && int64(len(raw)) > logStart {
			tail := string(raw[logStart:])
			if strings.Contains(tail, "现有浏览器会话") || strings.Contains(tail, "existing browser session") {
				m.killLocked()
				return errors.New("profile 被占用：浏览器把启动请求转交给了已在运行的实例")
			}
		}
		if probePort(port) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	wsURL, version, verr := waitForDebugger(ctx, port, 14*time.Second)
	if verr != nil {
		m.killLocked()
		return verr
	}
	client, derr := cdp.Dial(ctx, wsURL)
	if derr != nil {
		m.killLocked()
		return fmt.Errorf("连接 CDP 失败: %w", derr)
	}
	m.client = client
	m.version = version
	m.recordSnapshotLocked()
	return nil
}

// killLocked 结束当前浏览器进程并清理连接（调用方需持有 m.mu）。
func (m *Manager) killLocked() {
	if m.client != nil {
		_ = m.client.Close()
		m.client = nil
	}
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		done := make(chan struct{})
		go func() {
			_, _ = m.cmd.Process.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
		}
		m.cmd = nil
		// 等进程真正退出，释放 user-data-dir 锁，否则下一次启动会因"profile in use"直接失败
		time.Sleep(600 * time.Millisecond)
	}
}

// ProbeRenderer 打开一个标签页做真实页面探测：能拿到 document.title 才算渲染进程活着。
// 这是"浏览器窗口打不开网页"的判定依据，也是启动自愈的依据。
func (m *Manager) ProbeRenderer(ctx context.Context, url string) Probe {
	m.mu.Lock()
	client, port := m.client, m.port
	m.mu.Unlock()
	return probeWith(ctx, client, port, url)
}

// probeWith 页面探测的实际实现（不加锁，供 Ensure 在持锁状态下调用）。
func probeWith(ctx context.Context, client *cdp.Client, port int, url string) Probe {
	started := time.Now()
	if client == nil {
		return Probe{OK: false, Error: "浏览器未启动"}
	}
	if url == "" {
		url = "about:blank"
	}
	pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := client.Call(pctx, "", "Target.createTarget", map[string]any{
		"url": url, "background": true,
	}, &created); err != nil {
		return Probe{OK: false, Error: "新建标签页失败：" + err.Error(), DurationMS: time.Since(started).Milliseconds()}
	}
	defer func() {
		_ = client.Call(context.Background(), "", "Target.closeTarget", map[string]any{"targetId": created.TargetID}, nil)
	}()

	pageWS := fmt.Sprintf("ws://127.0.0.1:%d/devtools/page/%s", port, created.TargetID)
	pc, err := cdp.Dial(pctx, pageWS)
	if err != nil {
		return Probe{OK: false, Error: "连接标签页失败：" + err.Error(), DurationMS: time.Since(started).Milliseconds()}
	}
	defer pc.Close()

	if err := pc.Call(pctx, "", "Page.enable", nil, nil); err != nil {
		return Probe{
			OK: false, Error: "渲染进程不可用（Page.enable 失败）：" + err.Error(),
			DurationMS: time.Since(started).Milliseconds(),
		}
	}
	// 等页面真正加载完（最多 6s），否则读到的 title 是空的，诊断信息没有参考价值
	var out struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	// 用 |@| 作分隔符：URL、标题、readyState 三段，全 ASCII，避免多层转义踩坑
	expr := "location.href + '|@|' + document.title + '|@|' + document.readyState"
	waitUntil := time.Now().Add(6 * time.Second)
	for {
		out = struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		}{}
		_ = pc.Call(pctx, "", "Runtime.evaluate", map[string]any{
			"expression": expr, "returnByValue": true,
		}, &out)
		val := out.Result.Value
		loaded := strings.HasSuffix(val, "|@|complete")
		// about:blank 也会立刻 complete，必须确认已经真的导航到目标地址
		if url != "about:blank" {
			shot := strings.SplitN(val, "|@|", 2)[0]
			loaded = loaded && shot != "" && shot != "about:blank"
		}
		if loaded || time.Now().After(waitUntil) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	parts := strings.Split(out.Result.Value, "|@|")
	p := Probe{OK: true, DurationMS: time.Since(started).Milliseconds()}
	if len(parts) > 1 {
		p.Title = parts[1]
	}
	if len(parts) > 2 {
		p.ReadyState = parts[2]
	}
	return p
}

// FindKernelExecutable 在指定内核目录里找可执行文件（自带内核用）。
func FindKernelExecutable(dir string) string {
	if dir == "" {
		return ""
	}
	candidates := []string{
		filepath.Join(dir, "chrome.exe"),
		filepath.Join(dir, "msedge.exe"),
		filepath.Join(dir, "chromium.exe"),
		filepath.Join(dir, "chrome-win64", "chrome.exe"),
		filepath.Join(dir, "chrome-win", "chrome.exe"),
		filepath.Join(dir, "chrome-linux64", "chrome"),
		filepath.Join(dir, "chrome-mac-x64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	// 兜底：递归找一层子目录里的 chrome.exe
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, name := range []string{"chrome.exe", "msedge.exe", "chromium.exe", "chrome"} {
			c := filepath.Join(dir, e.Name(), name)
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c
			}
		}
	}
	return ""
}

// resolveExeLocked 解析要用的浏览器可执行文件（调用方需持有 m.mu）。
// 顺序：自带内核目录 → 设置里显式指定 → 环境变量/系统安装探测。
func (m *Manager) resolveExeLocked() (string, error) {
	if m.kernelDir != "" {
		if p := FindKernelExecutable(m.kernelDir); p != "" {
			m.exe = p
			return p, nil
		}
	}
	if m.exe != "" {
		if _, err := os.Stat(m.exe); err == nil {
			return m.exe, nil
		}
	}
	p, err := FindExecutable("")
	if err != nil {
		return "", err
	}
	m.exe = p
	return p, nil
}

// SetResolveHook 注册"探测成功"的回调，用于把可用启动参数持久化。
func (m *Manager) SetResolveHook(fn func(flags []string, profileDir string)) {
	m.mu.Lock()
	m.onResolve = fn
	m.mu.Unlock()
}

// adoptProfile 尝试复用「上一次运行遗留、但进程还活着」的浏览器实例。
// Chromium 会把调试端口写进 <user-data-dir>/DevToolsActivePort，据此可以直接接管，
// 避免 user-data-dir 被占用导致新实例启动即退出、调试端口等不到的失败。
func adoptProfile(ctx context.Context, profileDir string) (*cdp.Client, int, string, error) {
	raw, err := os.ReadFile(filepath.Join(profileDir, "DevToolsActivePort"))
	if err != nil {
		return nil, 0, "", err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 {
		return nil, 0, "", fmt.Errorf("DevToolsActivePort 内容为空")
	}
	port := 0
	for _, c := range strings.TrimSpace(lines[0]) {
		if c < '0' || c > '9' {
			return nil, 0, "", fmt.Errorf("DevToolsActivePort 端口非法: %s", lines[0])
		}
		port = port*10 + int(c-'0')
	}
	if port <= 0 {
		return nil, 0, "", fmt.Errorf("DevToolsActivePort 端口非法")
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	wsURL, version, err := probeDebugger(ctx, base, 3*time.Second)
	if err != nil {
		return nil, 0, "", err
	}
	client, err := cdp.Dial(ctx, wsURL)
	if err != nil {
		return nil, 0, "", err
	}
	return client, port, version, nil
}

// attemptProfileOr 本次尝试要用的 user-data-dir（重试时用独立子目录）。
func (m *Manager) attemptProfileOr() string {
	if m.attemptProfile != "" {
		return m.attemptProfile
	}
	return m.profileDir
}

// cleanStaleRetryProfiles 清理历史上重试用的临时 profile 目录（尽力而为，失败忽略）。
// 这些目录里的 SingletonLock 若残留，会让新启动的浏览器"委托给已有会话"直接退出。
func (m *Manager) cleanStaleRetryProfiles() {
	matches, err := filepath.Glob(m.profileDir + "-retry*")
	if err != nil {
		return
	}
	for _, dir := range matches {
		// 已固化的正式 profile 目录不能删（登录态在里面）
		if dir == m.profileDir {
			continue
		}
		_ = os.RemoveAll(dir)
	}
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// clearStaleProfileLocks 清掉 user-data-dir 里残留的单实例锁文件。
//
// 何时安全：已经确认这个 profile 没有活着的调试端口（adoptProfile 失败）时，说明持有锁的
// 进程已经没了；此时不清锁，新启动的 Chromium 会打印「正在现有浏览器会话中打开」然后直接退出，
// 调试端口永远等不到，只好换新目录——那样又会丢掉登录态。清锁后可以继续用同一个目录。
func clearStaleProfileLocks(profileDir string) {
	if profileDir == "" {
		return
	}
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie", "lockfile", "DevToolsActivePort"} {
		p := filepath.Join(profileDir, name)
		if st, err := os.Lstat(p); err == nil {
			if st.IsDir() {
				_ = os.RemoveAll(p)
			} else {
				_ = os.Remove(p)
			}
		}
	}
}

// probePort 调试端口是否已经有响应。
func probePort(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// retrySuffixRe 匹配重试后缀：-retry1-123456
var retrySuffixRe = regexp.MustCompile(`-retry\d+-\d+$`)

// stripRetrySuffix 去掉目录名上累积的重试后缀。
// 成功启动后会把 "-retryN-xxx" 目录固化成正式 profile，如果不剥离，
// 每次重启都会在名字上再叠一层，最后长成这样：
//
//	browser-profile-retry1-134100-retry1-955600-retry2-…
func stripRetrySuffix(p string) string {
	for {
		next := retrySuffixRe.ReplaceAllString(p, "")
		if next == p {
			return p
		}
		p = next
	}
}

// adoptAnySibling 在数据目录里找"还活着的"浏览器实例并接管。
// 场景：上次进程被强杀留下孤儿子进程，它仍占着某个 profile 目录并用着调试端口；
// 此时直接接管比重新拉起更快，也不会因为 profile 被占用而启动失败。
func adoptAnySibling(ctx context.Context, baseProfileDir string) (*cdp.Client, int, string, string, error) {
	parent := filepath.Dir(baseProfileDir)
	prefix := filepath.Base(baseProfileDir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, 0, "", "", err
	}
	// 优先干净的正式目录，其次才是各种 retry 目录
	cands := []string{}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		cands = append(cands, filepath.Join(parent, e.Name()))
	}
	sort.Slice(cands, func(i, j int) bool { return len(cands[i]) < len(cands[j]) })
	for _, dir := range cands {
		client, port, version, aerr := adoptProfile(ctx, dir)
		if aerr != nil {
			continue
		}
		if p := probeWith(ctx, client, port, "about:blank"); p.OK {
			return client, port, version, dir, nil
		}
		_ = client.Close()
	}
	return nil, 0, "", "", errors.New("没有可接管的活实例")
}
