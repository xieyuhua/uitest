// 临时工具：无头截图 + 可点击某个选择器（验证弹窗，不进交付物）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"uitest-server/internal/browser"
	"uitest-server/internal/model"
)

func main() {
	base := flag.String("base", "http://127.0.0.1:8491", "控制台地址")
	path := flag.String("path", "/#/collect", "hash 路由")
	out := flag.String("out", "data/tmp/shot.png", "截图输出")
	click := flag.String("click", "", "渲染后点击的选择器")
	wait := flag.Int("wait", 2600, "首次等待毫秒")
	after := flag.Int("after", 1400, "点击后等待毫秒")
	flag.Parse()

	cfg := model.DefaultSettings().Browser
	cfg.Headless = true
	mgr := browser.NewManager(cfg, filepath.Join(os.TempDir(), "uicheck6-profile"), filepath.Join(os.TempDir(), "uicheck6-logs"))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	page, err := mgr.OpenPage(ctx, "about:blank")
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer page.Close(context.Background())
	defer mgr.Close()
	if err := page.Navigate(ctx, *base+*path); err != nil {
		fmt.Println("nav:", err)
		os.Exit(1)
	}
	time.Sleep(time.Duration(*wait) * time.Millisecond)
	if *click != "" {
		if _, err := page.Eval(ctx, `(function(){var el=document.querySelector(`+fmt.Sprintf("%q", *click)+`); if(!el) return 'no-el'; el.click(); return 'clicked';})()`); err != nil {
			fmt.Println("click:", err)
		}
		time.Sleep(time.Duration(*after) * time.Millisecond)
	}
	var rep struct {
		Modal    int    `json:"modal"`
		Title    string `json:"title"`
		Lines    []string `json:"lines"`
	}
	_ = page.EvalInto(ctx, `(function(){
		var m = document.querySelector('.modal, .modal-overlay .modal');
		var checks = document.querySelectorAll('.modal input[type=checkbox]');
		var labels = [];
		Array.prototype.forEach.call(document.querySelectorAll('.modal label.check-line'), function(l){ labels.push(l.innerText.trim()); });
		return {modal: checks.length, title: m? (m.innerText||'').split('\n')[0] : '', lines: labels};
	})()`, &rep)
	fmt.Printf("modalCheckboxes=%d title=%q\n", rep.Modal, rep.Title)
	for _, l := range rep.Lines {
		fmt.Println("  -", l)
	}
	_ = os.MkdirAll(filepath.Dir(*out), 0o755)
	if shot, e := page.Screenshot(ctx); e == nil {
		_ = os.WriteFile(*out, shot, 0o644)
		fmt.Println("shot:", *out)
	}
}
