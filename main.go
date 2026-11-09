// uitest-server：定时接口采集 + UI 操作回放平台的 Go 服务端。
//
//   - 内置浏览器：以独立用户目录启动 Edge/Chrome，通过 CDP 驱动；
//   - 录制：浏览器插件把操作步骤（含稳定 data-uitest-id 定位）上报到这里；
//   - 执行：先用 AI/规则生成数据准备，再把数据注入录制步骤跑真实浏览器；
//   - 观测：自带的网页控制台可以看到每条录制、每次运行、逐步日志与失败截图。
//
// 启动：go run . 或 go build -o uitest-server.exe . 后运行；
// 监听地址取 config.json 的 server.host（默认 0.0.0.0，局域网可访问），
// 只想本机用就填 127.0.0.1，也可用启动参数 -host / -port 覆盖。
package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"uitest-server/internal/api"
	"uitest-server/internal/browser"
	"uitest-server/internal/bus"
	"uitest-server/internal/collect"
	"uitest-server/internal/config"
	"uitest-server/internal/demoapi"
	"uitest-server/internal/engine"
	"uitest-server/internal/model"
	"uitest-server/internal/store"
)

// Version 服务端版本号。
const Version = "1.0.0"

//go:embed all:web
var webFS embed.FS

//go:embed all:demo
var demoFS embed.FS

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	port := flag.Int("port", 0, "覆盖监听端口（默认取配置文件，8490）")
	host := flag.String("host", "", "覆盖监听地址（默认取配置文件；0.0.0.0 = 允许局域网/容器外访问）")
	headless := flag.Bool("headless", false, "内置浏览器使用无头模式")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[uitest] ")

	cfg := config.Load(resolveConfigPath(*configPath))
	if *port > 0 {
		cfg.Server.Port = *port
	}
	if strings.TrimSpace(*host) != "" {
		cfg.Server.Host = strings.TrimSpace(*host)
	}
	if *headless {
		cfg.Browser.Headless = true
	}

	dataDir := config.AbsDataDir(cfg.Storage.Dir)
	db, err := store.Open(dataDir, cfg.Storage.KeepRuns)
	if err != nil {
		log.Fatalf("打开数据目录失败: %v", err)
	}
	defer db.Close()

	// 配置合并口径：设置里为空/零值时才采用 config.json 的值，
	// 避免覆盖用户在控制台改过的设置（曾经的 bug：no_sandbox / debug_url 没被合并，
	// 导致配置里写了也不生效）。
	st := db.Settings()
	def := model.DefaultSettings()
	changed := false
	mergeStr := func(dst *string, src string) {
		if *dst == "" && src != "" {
			*dst = src
			changed = true
		}
	}
	mergeStr(&st.AI.BaseURL, cfg.AI.BaseURL)
	mergeStr(&st.AI.APIKey, cfg.AI.APIKey)
	mergeStr(&st.AI.Model, cfg.AI.Model)
	if st.AI.TimeoutSec == 0 {
		st.AI.TimeoutSec = cfg.AI.TimeoutSec
		changed = true
	}
	mergeStr(&st.Browser.Executable, cfg.Browser.Executable)
	mergeStr(&st.Browser.KernelDir, cfg.Browser.KernelDir)
	mergeStr(&st.Browser.DebugURL, cfg.Browser.DebugURL)
	if st.Browser.StepDelayMS == 0 {
		st.Browser.StepDelayMS = cfg.Browser.StepDelayMS
		changed = true
	}
	if st.Browser.ActionTimeoutMS == 0 {
		st.Browser.ActionTimeoutMS = cfg.Browser.ActionTimeoutMS
		changed = true
	}
	if cfg.Browser.Headless && !st.Browser.Headless {
		st.Browser.Headless = true
		changed = true
	}
	if cfg.Browser.NoSandbox && !st.Browser.NoSandbox {
		st.Browser.NoSandbox = true
		changed = true
	}
	if len(st.Browser.ExtraArgs) == 0 && len(cfg.Browser.ExtraArgs) > 0 {
		st.Browser.ExtraArgs = append([]string(nil), cfg.Browser.ExtraArgs...)
		changed = true
	}
	if len(st.Browser.ResolvedFlags) == 0 && len(cfg.Browser.ResolvedFlags) > 0 {
		st.Browser.ResolvedFlags = append([]string(nil), cfg.Browser.ResolvedFlags...)
		changed = true
	}
	if st.Browser.StepDelayMS <= 0 {
		st.Browser.StepDelayMS = def.Browser.StepDelayMS
	}
	if changed {
		db.SaveSettings(st)
	}
	// 自带内核目录：设置里没填就用 <数据目录的上级>/browser（把 Chrome for Testing 解压到这里即可）
	kernelDir := st.Browser.KernelDir
	if kernelDir == "" {
		kernelDir = filepath.Join(filepath.Dir(dataDir), "browser")
	}
	browserCfg := st.Browser
	browserCfg.KernelDir = kernelDir
	log.Printf("[browser] 生效配置：kernel=%q profile=%q flags=%v debug_url=%q",
		browserCfg.KernelDir, browserCfg.ProfileDir, browserCfg.ResolvedFlags, browserCfg.DebugURL)
	browserMgr := browser.NewManager(browserCfg,
		filepath.Join(dataDir, "browser-profile"),
		filepath.Join(dataDir, "logs"))
	// 自动探测到可用启动参数后持久化，下次直接复用（避免每次都试错启动）
	browserMgr.SetResolveHook(func(flags []string, profileDir string) {
		cur := db.Settings()
		cur.Browser.ResolvedFlags = append([]string(nil), flags...)
		cur.Browser.ProfileDir = profileDir
		db.SaveSettings(cur)
		if len(flags) == 0 {
			log.Printf("[browser] 启动参数自适应结果：默认参数可用")
		} else {
			log.Printf("[browser] 启动参数自适应结果：%s（已记住，下次直接复用）", strings.Join(flags, " "))
		}
	})
	eventBus := bus.New()
	runner := engine.New(db, browserMgr, eventBus)
	// 接口采集：录制 → 筛选 → 定时抓取 → 落库（调度器随服务一起启动）
	collectSvc := collect.NewService(db, browserMgr, eventBus, dataDir)
	// 采集任务可以配「前置登录」：跑一段 UI 自动化把内置浏览器会话刷新好再抓
	collectSvc.Harvester.SetPreloginRunner(runner)
	collectSvc.Start()
	defer collectSvc.Close()
	apiSrv := api.New(db, browserMgr, runner, eventBus, collectSvc, Version, cfg.Server.Port)

	root := http.NewServeMux()
	root.Handle("/api/", apiSrv.Handler())
	// 运行产物（截图等）：只允许按具体文件访问，目录一律 404，
	// 免得 /data/runs/ 直接把「跑过哪些用例、什么时候跑的」列给扫描者看。
	root.Handle("/data/runs/", http.StripPrefix("/data/runs/",
		http.FileServer(noDirFS{root: http.Dir(filepath.Join(dataDir, "runs"))})))
	// 带分页的示例接口（演练"自动抓取分页数据"用，数据是固定的 23 条）
	root.Handle("/demo/api/goods", demoapi.GoodsHandler())
	root.Handle("/demo/api/goods-offset", demoapi.GoodsOffsetHandler())
	root.Handle("/demo/api/goods-cursor", demoapi.GoodsCursorHandler())
	root.Handle("/demo/api/info", demoapi.InfoHandler())
	// 「需要登录」的自测接口：登录发 token + 种 Cookie，订单接口不带登录态直接 401
	root.Handle("/demo/api/login", demoapi.LoginHandler())
	root.Handle("/demo/api/orders", demoapi.OrdersHandler())
	if sub, err := fs.Sub(demoFS, "demo"); err == nil {
		root.Handle("/demo/", http.StripPrefix("/demo/", noCache(http.FileServer(http.FS(sub)))))
	}
	root.Handle("/", consoleHandler())

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           root,
		ReadHeaderTimeout: 15 * time.Second,
		// 不设 WriteTimeout：SSE 实时日志需要长连接
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		banner(cfg.Server.Host, cfg.Server.Port, dataDir, st)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务异常: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("收到退出信号，正在关闭…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	_ = browserMgr.Close()
	log.Println("已退出")
}

func banner(host string, port int, dataDir string, st model.Settings) {
	local := fmt.Sprintf("http://127.0.0.1:%d", port)
	exe, err := browser.FindExecutable(st.Browser.Executable)
	exeDesc := exe
	if err != nil {
		exeDesc = "未探测到（可在设置页手动填写）"
	}

	if isWildcardHost(host) {
		// 绑定全部网卡：本地仍然可用，同时把局域网地址列出来，省得自己去查 IP
		log.Printf("服务已启动：监听 %s:%d（全部网卡）", displayHost(host), port)
		log.Printf("  控制台      %s/", local)
		lan := lanURLs(port)
		if len(lan) == 0 {
			log.Printf("  局域网访问  未探测到可用网卡地址（用 ipconfig 看一下本机 IP，拼成 http://<本机IP>:%d）", port)
		} else {
			for _, u := range lan {
				log.Printf("  局域网访问  %s/", u)
			}
		}
		log.Printf("  安全提醒    %s", lanWarning())
	} else {
		base := fmt.Sprintf("http://%s:%d", host, port)
		log.Printf("服务已启动：%s", base)
		log.Printf("  控制台      %s/", base)
	}

	log.Printf("  示例被测页  %s/demo/   （可用来演练录制/执行）", local)
	log.Printf("  数据目录    %s", dataDir)
	log.Printf("  浏览器      %s", exeDesc)
	if st.AI.BaseURL == "" || st.AI.APIKey == "" {
		log.Printf("  AI 数据     未配置（使用内置规则生成，可在设置页填 base_url/api_key/model）")
	} else {
		log.Printf("  AI 数据     %s / %s", st.AI.BaseURL, st.AI.Model)
	}
	log.Printf("  插件录制    在 Edge/Chrome 中加载 uitest/extension 目录（详见 extension/README.md）")
}

// isWildcardHost 是否监听全部网卡（0.0.0.0 / [::] / 空）。
func isWildcardHost(h string) bool {
	switch strings.TrimSpace(h) {
	case "", "0.0.0.0", "::", "[::]", "*":
		return true
	}
	return false
}

// displayHost 日志里展示的监听地址。
func displayHost(h string) string {
	if strings.TrimSpace(h) == "" {
		return "0.0.0.0"
	}
	return strings.TrimSpace(h)
}

// resolveConfigPath 找配置文件：先按当前目录，再按可执行文件所在目录。
//
// 这样从别的目录启动 exe（快捷方式、计划任务、services）也能读到 config.json ——
// "改了 server.host 却没生效"多半就是启动目录不同导致的。
func resolveConfigPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), p)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return p
}

// lanURLs 列出本机非回环网卡的可访问地址（0.0.0.0 启动时提示局域网入口）。
func lanURLs(port int) []string {
	out := []string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			u := ""
			if v4 := ip.To4(); v4 != nil {
				u = fmt.Sprintf("http://%s:%d", v4.String(), port)
			} else {
				u = fmt.Sprintf("http://[%s]:%d", ip.String(), port)
			}
			if seen[u] {
				continue
			}
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// lanWarning 对外开放时的安全提醒（控制台没有登录鉴权）。
func lanWarning() string {
	return "控制台/接口没有登录鉴权，绑到全部网卡后同网段的人都能操作（含启动内置浏览器、改配置）；" +
		"只在可信网络使用，或用防火墙只放行你自己的 IP（Windows：netsh advfirewall firewall add rule ...）"
}

// consoleHandler 托管内嵌的控制台静态资源。
//
// 注意：前端用 hash 路由，未命中的路径一律回退 index.html。这里必须直接写出
// index.html 的内容，不能把 r.URL.Path 改写成 "/index.html" 再交给 http.FileServer
// ——后者对以 /index.html 结尾的请求会 301 到 "./"，从而形成重定向死循环。
func consoleHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "控制台静态资源缺失", http.StatusInternalServerError)
		})
	}
	indexHTML, readErr := fs.ReadFile(sub, "index.html")
	fileServer := http.FileServer(http.FS(sub))

	// embed.FS 的文件在进程生命周期内不会变（改前端要重新编译），
	// 所以可以按内容算 ETag：控制台刷新时静态资源直接 304，省掉几百 KB 传输
	// （app.js + collect.js + styles.css 加起来 300KB+）。
	etags := map[string]string{}
	_ = fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, e := fs.ReadFile(sub, path); e == nil {
			sum := sha256.Sum256(b)
			etags["/"+path] = `"` + hex.EncodeToString(sum[:8]) + `"`
		}
		return nil
	})

	serveIndex := func(w http.ResponseWriter) {
		if readErr != nil || len(indexHTML) == 0 {
			http.Error(w, "控制台 index.html 缺失：请确认 web/index.html 已随二进制打包", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			serveIndex(w)
			return
		}
		if st, statErr := fs.Stat(sub, p); statErr != nil || st.IsDir() {
			serveIndex(w)
			return
		}
		if strings.HasSuffix(p, ".html") {
			w.Header().Set("Cache-Control", "no-store")
		} else if tag, ok := etags[r.URL.Path]; ok {
			// 静态资源（js/css）：带 ETag 让浏览器缓存，但要每次回源校验，
			// 这样升级二进制后旧缓存一定失效。
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
			if etagMatches(r.Header.Get("If-None-Match"), tag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

// noDirFS 屏蔽目录：只允许打开具体文件，任何目录（含根目录）都按「不存在」处理。
//
// http.FileServer 遇到目录会返回 HTML 列表，这里直接让它 404 —— 控制台只需要
// 按 data/runs/<runID>/xxx.png 这样取具体截图，不需要目录浏览能力。
type noDirFS struct{ root http.FileSystem }

func (n noDirFS) Open(name string) (http.File, error) {
	f, err := n.root.Open(name)
	if err != nil {
		return nil, err
	}
	fi, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return nil, statErr
	}
	if fi.IsDir() {
		_ = f.Close()
		return nil, fs.ErrNotExist
	}
	return f, nil
}

// etagMatches 判断 If-None-Match 头里是否包含给定 ETag（支持 */逗号列表/W- 前缀）。
func etagMatches(header, tag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	want := strings.TrimPrefix(tag, "W/")
	for _, part := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(part), "W/") == want {
			return true
		}
	}
	return false
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
