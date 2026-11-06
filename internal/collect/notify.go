package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"uitest-server/internal/model"
)

// 本文件：把一次运行的结果推到企业微信群机器人。
//
// 用的是「群机器人」webhook（https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx），
// 不需要 access_token；推送失败只会记一条 warn 进运行日志，绝不影响采集结果本身。

// notifyTimeout 推送超时（企业微信正常 1s 内返回，给足 10s 兜住网络抖动）。
const notifyTimeout = 10 * time.Second

// wecomResp 群机器人返回体。
type wecomResp struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// SendWeCom 往群机器人发一条消息。
//
// 有 @ 人 时用 text（企业微信 markdown 不支持 @），否则用 markdown（样式更好看）。
func SendWeCom(ctx context.Context, webhook, content string, mentioned []string) error {
	webhook = strings.TrimSpace(webhook)
	if webhook == "" {
		return fmt.Errorf("Webhook 地址为空")
	}
	if !strings.HasPrefix(strings.ToLower(webhook), "http") {
		return fmt.Errorf("Webhook 地址格式不对（应以 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key= 开头）")
	}

	var payload any
	if len(mentioned) > 0 {
		payload = map[string]any{
			"msgtype": "text",
			"text": map[string]any{
				"content":        plainText(content),
				"mentioned_list": mentioned,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"content": content},
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("组装消息失败：%w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("请求企业微信失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	var out wecomResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("企业微信返回看不懂（HTTP %d）：%s", resp.StatusCode, ClipText(string(raw), 200))
	}
	if out.ErrCode != 0 {
		return fmt.Errorf("企业微信返回 errcode=%d：%s", out.ErrCode, out.ErrMsg)
	}
	return nil
}

// plainText 把 markdown 退化成纯文本（@ 人时企业微信用 text 消息）。
func plainText(md string) string {
	s := md
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "### ", "")
	s = strings.ReplaceAll(s, "## ", "")
	s = strings.ReplaceAll(s, "# ", "")
	s = strings.ReplaceAll(s, "> ", "")
	s = strings.ReplaceAll(s, "`", "")
	// [文案](链接) → 文案 链接
	for {
		i := strings.Index(s, "](")
		if i < 0 {
			break
		}
		open := strings.LastIndex(s[:i], "[")
		if open < 0 {
			break
		}
		close := strings.Index(s[i:], ")")
		if close < 0 {
			break
		}
		text := s[open+1 : i]
		link := s[i+2 : i+close]
		s = s[:open] + text + " " + link + s[i+close+1:]
	}
	// <font color="warning">x</font> → x
	for {
		start := strings.Index(s, "<font")
		if start < 0 {
			break
		}
		gt := strings.Index(s[start:], ">")
		if gt < 0 {
			break
		}
		end := strings.Index(s[start:], "</font>")
		if end < 0 {
			break
		}
		s = s[:start] + s[start+gt+1:start+end] + s[start+end+len("</font>"):]
	}
	return strings.TrimSpace(s)
}

// WeComMessage 拼一条运行结果消息（markdown）。
func WeComMessage(task model.HarvestTask, run model.HarvestRun, status, message string, cfg model.NotifySettings) string {
	okCount, failCount := 0, 0
	var failedDetail []string // 失败接口 + 原因（最多 3 条）
	var topWrote []string     // 成功时：写了几行的接口（最多 3 条）
	for _, e := range run.Endpoints {
		name := orDefaultStr(strings.TrimSpace(e.Name), e.URL)
		if strings.TrimSpace(e.Error) != "" {
			failCount++
			if len(failedDetail) < 3 {
				failedDetail = append(failedDetail, "「"+ClipText(name, 24)+"」"+ClipText(oneLine(e.Error), 80))
			}
			continue
		}
		okCount++
		if e.Written > 0 && len(topWrote) < 3 {
			topWrote = append(topWrote, ClipText(name, 24)+" "+strconv.Itoa(e.Written)+" 行")
		}
	}

	head := "# ✅ 接口采集成功"
	color := "info"
	if status == "failed" {
		head = "# ⚠️ 接口采集失败"
		color = "warning"
	}

	var b strings.Builder
	b.WriteString(head + "\n")
	b.WriteString("> 任务：**" + orDefaultStr(strings.TrimSpace(task.Name), task.ID) + "**\n")
	b.WriteString("> 状态：<font color=\"" + color + "\">" + notifyStatusText(status) + "</font>\n")
	b.WriteString("> 触发：" + triggerLabel(run.Trigger) + "\n")
	at := run.FinishedAt
	if at.IsZero() {
		at = time.Now()
	}
	b.WriteString("> 时间：" + at.Format("2006-01-02 15:04:05") + "\n")

	line := fmt.Sprintf("> 接口：%d 个（成功 %d / 失败 %d），请求 %d 次，写入 %d 行",
		len(run.Endpoints), okCount, failCount, run.Requests, run.Written)
	b.WriteString(line + "\n")
	if d := run.FinishedAt.Sub(run.StartedAt); d > 0 {
		b.WriteString("> 耗时：" + d.Round(time.Millisecond).String() + "\n")
	}
	if run.Sink != "" {
		b.WriteString("> 写入：" + run.Sink + "\n")
	}
	if status == "passed" && len(topWrote) > 0 {
		b.WriteString("> 明细：" + strings.Join(topWrote, "、") + "\n")
	}

	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = strings.TrimSpace(run.Error)
	}
	if status == "failed" && msg != "" {
		b.WriteString("> 错误：" + ClipText(oneLine(msg), 300) + "\n")
	}
	for _, d := range failedDetail {
		b.WriteString("> 失败：" + d + "\n")
	}

	if base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"); base != "" {
		b.WriteString("\n[查看运行日志](" + base + "/#/collect/runs/" + run.ID + ")\n")
	}
	return b.String()
}

// oneLine 把多行错误压成一行（群里好看）。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

func notifyStatusText(status string) string {
	switch status {
	case "passed":
		return "成功"
	case "failed":
		return "失败"
	case "canceled":
		return "已取消"
	default:
		return status
	}
}

// notifyRunAsync 运行结束后推送（异步，不拖慢 finish）。
//
// 通知配置来自系统设置（设置 → 通知），所有任务共用一份。
func (h *Harvester) notifyRunAsync(task model.HarvestTask, runID, status, message string) {
	cfg := h.store.Settings().Notify
	if !cfg.Active() {
		return
	}
	switch status {
	case "passed":
		if !cfg.WantSuccess() {
			return
		}
	case "failed":
		if !cfg.WantFailure() {
			return
		}
	default:
		return // 取消不推
	}

	go func() {
		run, err := h.store.GetHarvestRun(runID)
		if err != nil {
			h.logf(runID, "warn", "企业微信通知跳过：读不到运行记录（%v）", err)
			return
		}
		content := WeComMessage(task, run, status, message, cfg)
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		if err := SendWeCom(ctx, cfg.Webhook, content, cfg.Mentioned); err != nil {
			h.logf(runID, "warn", "企业微信通知失败：%v", err)
			return
		}
		h.logf(runID, "info", "已推送企业微信通知（%s）", notifyStatusText(status))
	}()
}

// WeComTestMessage 设置页「发一条测试」用的消息。
func WeComTestMessage(cfg model.NotifySettings) string {
	return WeComMessage(model.HarvestTask{ID: "test", Name: "（测试消息，不是真实任务）"}, model.HarvestRun{
		ID:         "test",
		TaskName:   "（测试消息）",
		Status:     "passed",
		Trigger:    "manual",
		StartedAt:  time.Now().Add(-3 * time.Second),
		FinishedAt: time.Now(),
		Requests:   3,
		Fetched:    60,
		Written:    60,
		Sink:       "SQLite 表 demo",
		Endpoints:  []model.HarvestEndpointResult{{Name: "测试接口", Fetched: 60, Written: 60}},
	}, "passed", "这是一条测试消息：配置没问题，正式运行结束后就会这样推给你", cfg)
}
