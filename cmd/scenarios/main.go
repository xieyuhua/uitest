// Command scenarios 一次性验证所有示例场景。
//
// 用法（服务端需已启动）：
//
//	go run ./cmd/scenarios              # 跑全部场景
//	go run ./cmd/scenarios -only 登录    # 只跑名字含"登录"的场景
//	go run ./cmd/scenarios -keep         # 保留生成的录制，方便在控制台看
//	go run ./cmd/scenarios -base http://127.0.0.1:8490
//
// 每个场景都会：新建录制 → 提交步骤（语义定位：near / role / cell / attr / placeholder）→
// 执行 → 诊断失败步骤。跨版本场景还会先「一键刷新定位」再换页面回放。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

var (
	baseURL = flag.String("base", "http://127.0.0.1:8490", "服务端地址")
	keep    = flag.Bool("keep", false, "保留生成的录制（默认跑完删除）")
	only    = flag.String("only", "", "只跑名字包含该字符串的场景")
)

/* ---------------------------------------------------------------- HTTP */

func apiCall(method, path string, body any) (map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, *baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 300 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("HTTP %d: %v", resp.StatusCode, out["error"])
	}
	return out, nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

/* ------------------------------------------------------------- 步骤构造 */

type step struct {
	Action     string         `json:"action"`
	Annotation string         `json:"annotation,omitempty"`
	Value      string         `json:"value,omitempty"`
	DataKey    string         `json:"data_key,omitempty"`
	Locator    map[string]any `json:"locator,omitempty"`
	Target     map[string]any `json:"target,omitempty"`
}

func nav(url string) step {
	return step{Action: "navigate", Annotation: "打开页面", Value: url,
		Locator: map[string]any{"strategy": "url", "value": url}}
}

// fill 用「表单项标题 → 同一表单项里的控件」定位并填值
func fill(label, value, key string) step {
	return step{
		Action:     "input",
		Annotation: "在【" + label + "】输入内容",
		Value:      value,
		DataKey:    key,
		Locator: map[string]any{"strategy": "near", "value": label + ">>input:not([type=hidden])",
			"fallbacks": []map[string]any{{"strategy": "label", "value": label}}},
		Target: map[string]any{"tag": "input", "type": "text", "label": label, "placeholder": "请输入" + label, "field_kind": "text"},
	}
}

func fillArea(label, value string) step {
	return step{
		Action:     "input",
		Annotation: "在【" + label + "】输入内容",
		Value:      value,
		DataKey:    "content",
		Locator:    map[string]any{"strategy": "near", "value": label + ">>textarea:not([type=hidden])"},
		Target:     map[string]any{"tag": "textarea", "label": label, "field_kind": "textarea"},
	}
}

func pick(label, value string) step {
	return step{
		Action:     "select",
		Annotation: "在【" + label + "】选择「" + value + "」",
		Value:      value,
		DataKey:    "opt",
		Locator:    map[string]any{"strategy": "near", "value": label + ">>select"},
		Target:     map[string]any{"tag": "select", "label": label, "field_kind": "select"},
	}
}

func checkByLabel(label string) step {
	return step{
		Action:     "check",
		Annotation: "勾选【" + label + "】",
		Locator:    map[string]any{"strategy": "near", "value": label + ">>input[type=checkbox]"},
		Target:     map[string]any{"tag": "input", "type": "checkbox", "label": label, "field_kind": "checkbox"},
	}
}

func clickRole(role, name string) step {
	return step{
		Action:     "click",
		Annotation: "点击【" + name + "】",
		Locator: map[string]any{"strategy": "role", "value": role + "|" + name,
			"fallbacks": []map[string]any{{"strategy": "text", "value": name}}},
		Target: map[string]any{"tag": "button", "role": role, "text": name, "label": name},
	}
}

func clickAttr(spec, name string) step {
	return step{
		Action:     "click",
		Annotation: "点击【" + name + "】（走 data-testid）",
		Locator:    map[string]any{"strategy": "attr", "value": spec},
		Target:     map[string]any{"tag": "button", "text": name, "label": name, "test_attr": spec},
	}
}

func clickCell(row, btn string) step {
	return step{
		Action:     "click",
		Annotation: "点击「" + row + "」这一行的【" + btn + "】",
		Locator:    map[string]any{"strategy": "cell", "value": row + ">>" + btn},
		Target: map[string]any{"tag": "button", "text": btn, "label": btn,
			"column": "操作", "row_text": row},
	}
}

func uploadByLabel(label string) step {
	return step{
		Action:     "upload",
		Annotation: "上传文件到【" + label + "】",
		DataKey:    "file",
		Locator:    map[string]any{"strategy": "near", "value": label + ">>input[type=file]"},
		Target:     map[string]any{"tag": "input", "type": "file", "label": label, "field_kind": "file"},
	}
}

func placeholderFill(ph, value, key string) step {
	return step{
		Action:     "input",
		Annotation: "在占位为「" + ph + "」的输入框输入内容",
		Value:      value,
		DataKey:    key,
		Locator:    map[string]any{"strategy": "placeholder", "value": ph},
		Target:     map[string]any{"tag": "input", "type": "text", "placeholder": ph, "label": ph, "field_kind": "text"},
	}
}

func assertText(v string) step {
	return step{Action: "assert", Annotation: "断言页面出现「" + v + "」", Value: v}
}

// oldStyle 模拟"老录制"：只有 css 定位，没有 uid、没有语义兜底
func oldStyle(label, value, css string) step {
	return step{
		Action:     "input",
		Annotation: "在【" + label + "】输入内容",
		Value:      value,
		DataKey:    "field_25",
		Locator:    map[string]any{"strategy": "css", "value": css},
		Target:     map[string]any{"tag": "input", "type": "text", "label": label, "placeholder": "请输入" + label, "field_kind": "text"},
	}
}

/* --------------------------------------------------------------- 场景 */

type scenario struct {
	Name string
	URL  string
	// Steps 录制步骤；RebindThen 非空时，先刷新定位再把起始页/导航步骤改到 RebindThen 这个地址
	Steps      []step
	RebindThen string
	Note       string
}

func scenarios() []scenario {
	d := *baseURL + "/demo"
	return []scenario{
		{
			Name: "1 基础表单（新建服务商品）",
			URL:  d + "/",
			Steps: []step{
				nav(d + "/"),
				clickRole("button", "新建商品"),
				fill("商品名称", "示例商品-A", "product_name"),
				fill("价格", "199.5", "price"),
				fill("库存", "66", "stock"),
				pick("商品分类", "虚拟类"),
				fillArea("商品描述", "由场景脚本自动填写"),
				uploadByLabel("封面图片"),
				clickRole("button", "保存商品"),
				assertText("创建成功"),
			},
		},
		{
			Name: "2 登录页（勾选 / 断言提示）",
			URL:  d + "/login.html",
			Steps: []step{
				nav(d + "/login.html"),
				fill("登录账号", "kefu_admin", "account"),
				fill("登录密码", "secret123", "password"),
				checkByLabel("记住密码"),
				clickRole("button", "登 录"),
				assertText("登录成功，欢迎 kefu_admin"),
			},
		},
		{
			Name: "3 列表 / 搜索 / 行内操作",
			URL:  d + "/admin-list.html",
			Steps: []step{
				nav(d + "/admin-list.html"),
				placeholderFill("搜索商品名称", "标准版", "keyword"),
				clickAttr("data-testid=btn-search", "查询"),
				assertText("共 1 条"),
				clickCell("标准版服务", "编辑"),
				assertText("正在编辑：标准版服务"),
				clickRole("button", "取消"),
				// 搜索把表格过滤成只剩"标准版服务"，旗舰版那行被 display:none，
				// 隐藏行是不可点的，所以先点「重置」还原列表
				clickRole("button", "重置"),
				clickCell("旗舰版服务", "删除"),
				assertText("已删除：旗舰版服务"),
			},
		},
		{
			Name: "4 弹窗表单 / 日期 / 开关 / 校验",
			URL:  d + "/admin-dialog.html",
			Steps: []step{
				nav(d + "/admin-dialog.html"),
				clickRole("button", "新建活动"),
				clickRole("button", "保存"),
				assertText("请填写活动名称"),
				fill("活动名称", "双十二满减", "activity_name"),
				pick("活动类型", "满减"),
				fill("开始日期", "2026-12-01", "start_date"),
				fill("结束日期", "2026-12-12", "end_date"),
				clickRole("switch", "上架"),
				clickRole("button", "保存"),
				assertText("保存成功：双十二满减"),
			},
		},
		{
			Name: "5 多步向导 / 校验提示",
			URL:  d + "/wizard.html",
			Steps: []step{
				nav(d + "/wizard.html"),
				fill("联系人姓名", "张三", "contact"),
				clickRole("button", "下一步"),
				assertText("请填写手机号"),
				fill("手机号", "13800001111", "mobile"),
				fill("企业名称", "示例科技有限公司", "company"),
				clickRole("button", "下一步"),
				assertText("示例科技有限公司"),
				clickRole("button", "提交开通"),
				assertText("开通成功：示例科技有限公司"),
			},
		},
		{
			Name: "6 标签页 / 开关 / 富文本 / 上传",
			URL:  d + "/settings-tabs.html",
			Steps: []step{
				nav(d + "/settings-tabs.html"),
				fill("店铺名称", "客服从容店", "shop_name"),
				clickRole("switch", "营业中"),
				clickRole("tab", "高级设置"),
				clickRole("switch", "开启"),
				fillArea("欢迎语", "您好，客服马上为您服务~"),
				uploadByLabel("店铺 Logo"),
				clickRole("button", "保存设置"),
				assertText("已保存：客服从容店"),
			},
		},
		{
			Name:       "7 重新打包后仍命中（A 录制 → B 回放）",
			URL:        d + "/vue-v1.html",
			RebindThen: d + "/vue-v2.html",
			Note:       "老录制只有 css 定位；先「一键刷新定位」升级成 uid，再把页面换成另一个打包版本执行",
			Steps: []step{
				nav(d + "/vue-v1.html"),
				oldStyle("商品名称", "跨版本商品", "input.el-input__inner"),
				oldStyle("价格", "288", "input.el-input__inner"),
				oldStyle("库存", "12", "input.el-input__inner"),
				{Action: "select", Annotation: "在【商品分类】选择「服务类」", Value: "服务类", DataKey: "category",
					Locator: map[string]any{"strategy": "css", "value": "select.el-select__inner"},
					Target:  map[string]any{"tag": "select", "label": "商品分类", "field_kind": "select"}},
				{Action: "click", Annotation: "点击【保存商品】",
					Locator: map[string]any{"strategy": "text", "value": "保存商品"},
					Target:  map[string]any{"tag": "button", "text": "保存商品", "label": "保存商品", "role": "button"}},
				assertText("已保存：跨版本商品"),
			},
		},
	}
}

/* --------------------------------------------------------------- 主流程 */

func main() {
	flag.Parse()

	if err := ensureReady(); err != nil {
		fmt.Println("准备失败:", err)
		os.Exit(1)
	}

	all := scenarios()
	var results []bool
	fmt.Printf("%-38s %-8s %-12s %s\n", "场景", "结果", "步骤", "说明")
	fmt.Println(strings.Repeat("-", 96))

	for _, sc := range all {
		if *only != "" && !strings.Contains(sc.Name, *only) {
			continue
		}
		ok, detail := runScenario(sc)
		results = append(results, ok)
		state := "PASS"
		if !ok {
			state = "FAIL"
		}
		stepInfo := detail
		fmt.Printf("%-38s %-8s %-12s %s\n", sc.Name, state, stepInfo, truncate(sc.Note, 40))
		if !ok {
			printFailureDetail(sc.Name)
		}
	}

	fmt.Println(strings.Repeat("-", 96))
	passed := 0
	for _, r := range results {
		if r {
			passed++
		}
	}
	fmt.Printf("场景结果：%d/%d 通过\n", passed, len(results))
	if passed != len(results) {
		os.Exit(1)
	}
	fmt.Println("全部场景通过 ✅")
}

func ensureReady() error {
	h, err := apiCall("GET", "/api/health", nil)
	if err != nil {
		return fmt.Errorf("服务端没起来（%s）：%w", *baseURL, err)
	}
	if h["browser_running"] != true {
		fmt.Println("内置浏览器未运行，正在启动…")
		if _, err := apiCall("POST", "/api/browser/launch", map[string]any{}); err != nil {
			return fmt.Errorf("启动内置浏览器失败：%w", err)
		}
	}
	return nil
}

func runScenario(sc scenario) (bool, string) {
	rec, err := apiCall("POST", "/api/recordings", map[string]any{"name": sc.Name, "start_url": sc.URL})
	if err != nil {
		fmt.Println("  创建录制失败:", err)
		return false, "-"
	}
	recID := fmt.Sprint(rec["id"])
	cleanup := func() {
		if !*keep {
			_, _ = apiCall("DELETE", "/api/recordings/"+recID, nil)
		} else {
			fmt.Printf("  （保留录制 %s）\n", recID)
		}
	}

	for _, s := range sc.Steps {
		if _, err := apiCall("POST", "/api/recordings/"+recID+"/steps", s); err != nil {
			fmt.Println("  加步骤失败:", err)
			cleanup()
			return false, "-"
		}
	}

	// 跨版本场景：先刷新定位，再换页面
	if sc.RebindThen != "" {
		rep, err := apiCall("POST", "/api/recordings/"+recID+"/rebind", map[string]any{})
		if err != nil {
			fmt.Println("  刷新定位失败:", err)
			cleanup()
			return false, "-"
		}
		fmt.Printf("  [%s] %v\n", sc.Name, rep["message"])
		if err := switchStartURL(recID, sc.RebindThen); err != nil {
			fmt.Println("  切换起始页失败:", err)
			cleanup()
			return false, "-"
		}
	}

	run, err := apiCall("POST", "/api/runs", map[string]any{"recording_id": recID})
	if err != nil {
		fmt.Println("  创建运行失败:", err)
		cleanup()
		return false, "-"
	}
	runID := fmt.Sprint(run["id"])

	deadline := time.Now().Add(240 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(1200 * time.Millisecond)
		r, err := apiCall("GET", "/api/runs/"+runID, nil)
		if err != nil {
			continue
		}
		st := fmt.Sprint(r["status"])
		if st == "running" || st == "queued" || st == "" {
			continue
		}
		total := fmt.Sprint(r["total_steps"])
		passed := fmt.Sprint(r["passed_steps"])
		cleanup()
		if st == "passed" {
			return true, passed + "/" + total
		}
		return false, st + " " + passed + "/" + total
	}
	cleanup()
	return false, "timeout"
}

// switchStartURL 把录制起始页与导航步骤都换成新地址
func switchStartURL(recID, newURL string) error {
	if _, err := apiCall("PATCH", "/api/recordings/"+recID, map[string]any{"start_url": newURL}); err != nil {
		return err
	}
	rec, err := apiCall("GET", "/api/recordings/"+recID, nil)
	if err != nil {
		return err
	}
	steps, _ := rec["steps"].([]any)
	for _, s := range steps {
		m, _ := s.(map[string]any)
		if fmt.Sprint(m["action"]) == "navigate" {
			_, err := apiCall("PATCH", "/api/recordings/"+recID+"/steps/"+fmt.Sprint(m["id"]),
				map[string]any{"value": newURL})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// printFailureDetail 打印最近一次运行的逐步结果，方便定位哪一步挂了
func printFailureDetail(name string) {
	runs, err := apiCall("GET", "/api/runs?limit=1", nil)
	if err != nil {
		return
	}
	items, _ := runs["items"].([]any)
	if len(items) == 0 {
		return
	}
	first, _ := items[0].(map[string]any)
	runID := fmt.Sprint(first["id"])
	r, err := apiCall("GET", "/api/runs/"+runID, nil)
	if err != nil {
		return
	}
	fmt.Printf("    失败明细（run %s，%v）：\n", runID, r["error"])
	results, _ := r["step_results"].([]any)
	keys := make([]int, 0, len(results))
	byIdx := map[int]map[string]any{}
	for _, x := range results {
		m, _ := x.(map[string]any)
		idx := int(numOf(m["index"]))
		keys = append(keys, idx)
		byIdx[idx] = m
	}
	sort.Ints(keys)
	for _, idx := range keys {
		m := byIdx[idx]
		mark := "✓"
		if fmt.Sprint(m["status"]) != "passed" {
			mark = "✗"
		}
		fmt.Printf("      %s #%d %-8s %-30s %s\n", mark, idx, fmt.Sprint(m["action"]),
			truncate(fmt.Sprint(m["locator_used"]), 30), truncate(fmt.Sprint(m["message"]), 90))
	}
}

func numOf(v any) float64 {
	f, _ := v.(float64)
	return f
}
