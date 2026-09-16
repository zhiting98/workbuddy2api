// autotask.go 一键完成任务的动作实现。
//
// 任务动作表 autoActions 顺序即执行顺序（先解锁依赖项）。所有动作幂等：
// 已 claimed/已达标的任务直接跳过，不重复消耗上游配额。
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// autoAction 一个可自动化的任务动作。
type autoAction struct {
	TaskCode string // 目标任务 code
	Desc     string // 展示用说明
	Attempt  bool   // true = 尝试型（上游未证实可脚本化，跑了可能不点亮）
	run      func(r *Runner, a *auth.Auth) (string, error)
}

// autoActions 已实现的任务动作表（顺序即执行顺序：先解锁依赖项）。
var autoActions = []autoAction{
	{TaskCode: "chat_5", Desc: "上报 5 条对话活跃事件（自动补足差额）", run: runChat5},
	{TaskCode: "first_buddy", Desc: "上报解锁 → 领取第一只 Buddy（+300 分）", run: runFirstBuddy},
	{TaskCode: "Model_chat_GLM5.2", Desc: "接受任务 → glm-5.2 真实对话一次 → 对齐模型上报", run: runModelChat},
	{TaskCode: "RichMeow_Chat", Desc: "桌面指纹事件链上报（纯 API 可点亮）", run: runRichMeow},
	{TaskCode: "Buddy_App", Desc: "上报「进入 Buddy 应用」事件链", run: runBuddyApp},
	{TaskCode: "Buddy_App_QQ", Desc: "上报「进入企鹅教师助手」事件链", run: runBuddyApp},
	{TaskCode: "automation_1", Desc: "上报「定时任务创建」事件", run: runAutomationCreate},
	{TaskCode: "Library_read", Desc: "上报「读资料库介绍」事件", run: runLibraryRead},
	{TaskCode: "template_5", Desc: "上报「使用模板创建任务」事件组 ×5", run: runTemplateUse},
	{TaskCode: "playbook_prompt", Desc: "上报「灵感案例做同款发送 Prompt」事件组", run: runPlaybookPrompt},
	{TaskCode: "create_canvas", Desc: "上报「设计创意画布创建」事件组（+300 分）", run: runCreateCanvas},
	{TaskCode: "expert_5", Desc: "真实专家召唤+使用链 ×5", run: runExpertUse},
	{TaskCode: "Expert_team_use_3", Desc: "真实专家团召唤+使用链 ×3", run: runExpertTeamUse},
	{TaskCode: "Hp_Appearance", Desc: "设置主题 API + 皮肤生效事件", run: runAppearance},
	{TaskCode: "skill_1", Desc: "真实对话 + skill_info 技能加载事件", run: runSkillFresh},
	{TaskCode: "Expert_lighthouse", Desc: "真实轻量云专家召唤+使用链", run: runExpertLighthouse},
	{TaskCode: "black_cat", Desc: "夜猫子：夜间窗口内 glm-5.2 对话补足", Attempt: true, run: runBlackCat},
}

// autoActionFor 查任务对应的动作；无则返回 nil（不可自动化）。
func autoActionFor(code string) *autoAction {
	for i := range autoActions {
		if autoActions[i].TaskCode == strings.TrimSpace(code) {
			return &autoActions[i]
		}
	}
	return nil
}

// autoActionIndex 任务在 autoActions 中的顺序（队列执行按依赖序排；未知返回大值）。
func autoActionIndex(code string) int {
	for i := range autoActions {
		if autoActions[i].TaskCode == code {
			return i
		}
	}
	return 1 << 20
}

// runChat5 补足 chat_5 的进度：按差额上报 chat_request_send。
func runChat5(r *Runner, a *auth.Auth) (string, error) {
	t, err := r.taskByCode(a, "chat_5")
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("任务不存在")
	}
	target := t.Target
	if target <= 0 {
		target = 5
	}
	need := target - t.Current
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	for i := int64(0); i < need; i++ {
		cid := fmt.Sprintf("wb2api-chat5-%d-%d", time.Now().UnixMilli(), i)
		if err := r.up.ReportChatActivity(a, cid, ""); err != nil {
			return fmt.Sprintf("上报第 %d/%d 条失败: %v", i+1, need, err), nil
		}
		if i < need-1 {
			time.Sleep(reportGap)
		}
	}
	return fmt.Sprintf("已补报 %d 条对话事件", need), nil
}

// runFirstBuddy 领养：report（解锁前置）→ first。
func runFirstBuddy(r *Runner, a *auth.Auth) (string, error) {
	if err := r.up.ReportChatActivity(a, fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		return "", fmt.Errorf("前置上报: %w", err)
	}
	time.Sleep(reportGap)
	if err := r.up.BuddyAgreement(a); err != nil {
		return "", fmt.Errorf("同意协议: %w", err)
	}
	if err := r.up.BuddyFirst(a); err != nil {
		if upstream.IsBuddyTaskIncomplete(err) {
			return "前置已上报，但领养门槛未过（上游要求当日活跃），请稍后重试", nil
		}
		return "", fmt.Errorf("领取 Buddy: %w", err)
	}
	return "已领取 Buddy（+300 分 +8 能量）", nil
}

// runModelChat 完成 Model_chat_GLM5.2：accept → 真实对话 → 对齐模型上报。
func runModelChat(r *Runner, a *auth.Auth) (string, error) {
	const code, modelID, modelName = "Model_chat_GLM5.2", "glm-5.2", "GLM-5.2"
	if err := r.up.AcceptTasks(a, []string{code}); err != nil {
		log.Printf("panel: accept %s: %v（继续走行为链路）", code, err)
	}
	time.Sleep(reportGap)
	body, _ := json.Marshal(map[string]any{
		"model": modelID,
		"messages": []map[string]any{
			{"role": "user", "content": "hi，请回复一句话"},
		},
		"stream": true,
	})
	rc, status, respBody, err := r.up.ChatStream(a, body)
	if err != nil {
		return "", fmt.Errorf("对话请求: %w", err)
	}
	if status >= 400 {
		rc.Close()
		return "", fmt.Errorf("对话失败 http=%d: %s", status, truncateStr(string(respBody), 160))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	rc.Close()
	time.Sleep(reportGap)
	if err := r.up.ReportChatActivityModel(a, fmt.Sprintf("wb2api-glm52-%d", time.Now().UnixMilli()), "", modelID, modelName); err != nil {
		return "对话已完成，但进度上报失败：" + err.Error(), nil
	}
	return "已完成 glm-5.2 对话并上报", nil
}

// runRichMeow 完成 RichMeow_Chat（桌面端对话1次）。
func runRichMeow(r *Runner, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-rm-%d", ms)
	req := fmt.Sprintf("wb2api-rm-req-%d", ms)
	msg := fmt.Sprintf("req-%d-user", ms)
	events := upstream.DesktopChatSequence(conv, req, msg, "fast-model", "fast-model")
	if err := r.up.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已按桌面端指纹上报完整对话事件链", nil
}

// runBuddyApp 完成 Buddy_App / Buddy_App_QQ（进入 Buddy 应用）。
func runBuddyApp(r *Runner, a *auth.Auth) (string, error) {
	events := upstream.DesktopBuddyAppSequence("cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手")
	if err := r.up.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 buddyapp 进入五连事件（同时覆盖 Buddy_App 与 Buddy_App_QQ）", nil
}

// runAutomationCreate 完成 automation_1（设置自动化任务）。
func runAutomationCreate(r *Runner, a *auth.Auth) (string, error) {
	if err := r.up.ReportDesktopEvent(a, upstream.DesktopAutomationCreateEvent("wb2api 自动化")); err != nil {
		return "", err
	}
	return "已上报定时任务创建事件", nil
}

// runLibraryRead 完成 Library_read（体验资料库）。
func runLibraryRead(r *Runner, a *auth.Auth) (string, error) {
	const docURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if err := r.up.ReportWebEvent(a, "web_element_click", docURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", err
	}
	return "已上报资料库介绍阅读事件", nil
}

// runBlackCat 完成 black_cat（夜猫子）。
func runBlackCat(r *Runner, a *auth.Auth) (string, error) {
	if !upstream.InNightWindow(time.Now()) {
		return "当前不在 23:00–08:00 计数窗口，行为不计分；请在夜间窗口内执行", nil
	}
	need, err := r.up.BlackcatNeed(a)
	if err != nil {
		return "", err
	}
	if need <= 0 {
		return "进度已达标，无需补足", nil
	}
	ok, err := r.up.RunNightChats(a, int(need))
	if err != nil {
		return fmt.Sprintf("完成 %d/%d 次后中断: %v", ok, need, err), nil
	}
	return fmt.Sprintf("已完成 %d 次夜间对话并上报", ok), nil
}

// runSkillFresh 完成 skill_1（尝鲜热门技能）。
func runSkillFresh(r *Runner, a *auth.Auth) (string, error) {
	conv, req, err := r.up.DesktopChatWithExpert(a, "")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	msgID := "msg-" + req[len(req)-8:]
	events := upstream.DesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls"
		}
	}
	events = append(events, upstream.DesktopEvent{
		"eventCode":        "skill_info",
		"id":               "润泽小馆·日报撰写",
		"skillId":          "skill_2097350077599879168",
		"skillVersion":     "1.0.0",
		"toolStatus":       "success",
		"fileCount":        56,
		"source":           "workbuddy-desktop",
		"conversationId":   conv,
		"requestId":        req,
		"messageId":        msgID,
		"requestModelId":   "fast-model",
		"requestModelName": "fast-model",
		"traceId":          req,
	})
	if err := r.up.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("skill_info 事件: %w", err)
	}
	return "已上报真实对话 + skill_info 技能加载事件", nil
}

// runExpertLighthouse 完成 Expert_lighthouse（体验「腾讯轻量云」专家）。
func runExpertLighthouse(r *Runner, a *auth.Auth) (string, error) {
	const lhID = "ex_2cvvUZQhDyeJ"
	lh := upstream.MarketExpert{
		ExpertID: lhID, ExpertType: "agent",
		DisplayNameZH: "腾讯轻量云专家", ProfessionZH: "腾讯轻量云专家", Version: "1.0.2",
	}
	if experts, err := r.up.MarketExpertList(a, "agent"); err == nil {
		for _, e := range experts {
			if e.ExpertID == lhID {
				lh = e
				break
			}
		}
	}
	if err := r.up.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("召唤链: %w", err)
	}
	conv, req, err := r.up.DesktopChatWithExpert(a, lhID)
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	events := upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ExpertID
			ev["expert_name"] = lh.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	events = append(events, upstream.DesktopExpertActualUseLocal(lh, conv, req))
	events[len(events)-1]["type"] = ""
	events[len(events)-1]["cost"] = 0
	if err := r.up.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("使用事件: %w", err)
	}
	return "已上报轻量云专家召唤+使用链（真实对话 requestId）", nil
}

// runAppearance 完成 Hp_Appearance（换主题）。
func runAppearance(r *Runner, a *auth.Auth) (string, error) {
	const themeKey = "theme-tkmw7j"
	if err := r.up.SetAppearanceTheme(a, themeKey); err != nil {
		return "", fmt.Errorf("设置主题: %w", err)
	}
	time.Sleep(2 * time.Second)
	if err := r.up.ReportDesktopEvent(a, upstream.DesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": themeKey, "vipLevel": 0, "series": "", "type": "unknown",
	}); err != nil {
		return "", err
	}
	return "已设置主题并上报皮肤生效事件", nil
}

// runTemplateUse 完成 template_5（使用 5 个模板创建任务）。
func runTemplateUse(r *Runner, a *auth.Auth) (string, error) {
	templates := [][2]string{{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"}, {"4", "活动策划"}, {"5", "代码评审"}}
	for i, tp := range templates {
		ms := time.Now().UnixMilli()
		conv := fmt.Sprintf("wb2api-tpl-%d-%d", ms, i)
		req := fmt.Sprintf("wb2api-tpl-req-%d-%d", ms, i)
		events := upstream.DesktopTemplateUseSequence(conv, req, tp[0], tp[1])
		if err := r.up.ReportDesktopEvent(a, events...); err != nil {
			return fmt.Sprintf("第 %d 组模板事件上报失败: %v", i+1, err), nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "已上报 template_used ×5", nil
}

// runPlaybookPrompt 完成 playbook_prompt（灵感案例 Dialog 中发送 Prompt）。
func runPlaybookPrompt(r *Runner, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-pb-%d", ms)
	req := fmt.Sprintf("wb2api-pb-req-%d", ms)
	events := upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")
	if err := r.up.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 playbook_cta_click + playbook_prompt_send", nil
}

// runCreateCanvas 完成 create_canvas（设计创意模式创建画布，+300 分）。
func runCreateCanvas(r *Runner, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-canvas-%d", ms)
	req := fmt.Sprintf("wb2api-canvas-req-%d", ms)
	events := upstream.DesktopDesignCanvasSequence(conv, req)
	if err := r.up.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 wbx_design_canvas_task_create/open", nil
}

// expertSummonGap 专家召唤链的间隔（真实使用节奏）。
const expertSummonGap = 6 * time.Second

// runExpertUse 完成 expert_5（使用 5 个平台专家）。
func runExpertUse(r *Runner, a *auth.Auth) (string, error) {
	return runExpertBatch(r, a, "agent", 5)
}

// runExpertTeamUse 完成 Expert_team_use_3（使用 3 个专家团）。
func runExpertTeamUse(r *Runner, a *auth.Auth) (string, error) {
	return runExpertBatch(r, a, "team", 3)
}

// runExpertBatch 专家召唤+使用的公共实现。失败逐个继续，返回汇总信息。
func runExpertBatch(r *Runner, a *auth.Auth, expertType string, count int) (string, error) {
	experts, err := r.up.MarketExpertList(a, expertType)
	if err != nil {
		return "", fmt.Errorf("拉取专家列表: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场列表为空")
	}
	ok := 0
	for i, e := range experts {
		if ok >= count {
			break
		}
		if err := r.up.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(e)...); err != nil {
			continue
		}
		conv, req, cerr := r.up.DesktopChatWithExpert(a, e.ExpertID)
		if cerr != nil {
			continue
		}
		events := append(upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model"),
			upstream.DesktopExpertActualUseEvent(e, conv, req))
		if err := r.up.ReportDesktopEvent(a, events...); err != nil {
			continue
		}
		ok++
		if i < len(experts)-1 {
			time.Sleep(expertSummonGap)
		}
	}
	return fmt.Sprintf("已对 %d 位真实专家完成召唤+使用链（类型 %s）", ok, expertType), nil
}

// runAutoAll 对单账号依次执行所有可自动化任务，返回逐项结果。单项失败不影响后续项。
func (r *Runner) runAutoAll(a *auth.Auth) []map[string]any {
	var out []map[string]any

	// 阶段 0：批量接受尚未接受的任务（失败不阻塞）。
	if tasks, err := r.up.ListTasks(a); err == nil {
		var codes []string
		for _, t := range tasks {
			if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
				codes = append(codes, t.TaskCode)
			}
		}
		if len(codes) > 0 {
			if err := r.up.AcceptTasks(a, codes); err != nil {
				out = append(out, map[string]any{
					"task_code": "(批量接受)", "status": "error",
					"message": "接受任务失败（不阻塞后续）: " + err.Error(),
				})
			} else {
				out = append(out, map[string]any{
					"task_code": "(批量接受)", "status": "done",
					"message": fmt.Sprintf("已接受 %d 个任务", len(codes)),
				})
				time.Sleep(reportGap)
			}
		}
	}

	for _, act := range autoActions {
		item := map[string]any{"task_code": act.TaskCode, "desc": act.Desc}
		before, err := r.taskByCode(a, act.TaskCode)
		if err != nil {
			item["status"] = "error"
			item["message"] = "查询失败: " + err.Error()
			out = append(out, item)
			continue
		}
		if before == nil {
			item["status"] = "skipped"
			item["message"] = "该账号无此任务"
			out = append(out, item)
			continue
		}
		if before.Claimed || before.Current >= before.Target && before.Target > 0 {
			item["status"] = "skipped"
			item["message"] = "已完成（" + taskProgressText(before) + "）"
			out = append(out, item)
			continue
		}
		msg, err := act.run(r, a)
		if err != nil {
			item["status"] = "error"
			item["message"] = err.Error()
			out = append(out, item)
			continue
		}
		after, _ := r.taskByCodeWaiting(a, act.TaskCode)
		item["status"] = "done"
		item["message"] = msg
		item["progress_after"] = taskProgressText(after)
		if after != nil && after.Claimable {
			item["claimable"] = true
			if credit, energy, cerr := r.up.ClaimReward(a, act.TaskCode); cerr == nil {
				item["claimed"] = true
				item["credit"] = credit
				item["energy"] = energy
				if credit > 0 || energy > 0 {
					item["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
				} else {
					item["message"] = msg + "；奖励此前已领取"
				}
			} else {
				item["claim_error"] = cerr.Error()
				item["message"] = msg + "；达标但领奖失败（可在列表手动重试）"
			}
		}
		out = append(out, item)
		time.Sleep(reportGap)
	}
	return out
}

// RunAuto 单账号一键完成全部可自动任务（per-account 串行，重复触发返回 engaged）。
// 返回 (results, engaged)。engaged=true 表示该账号已有任务动作在跑。
func (r *Runner) RunAuto(uid string) (results []map[string]any, engaged bool) {
	a := r.accountByUID(uid)
	if a == nil {
		return nil, false
	}
	if !r.tryLockAccount(uid) {
		return nil, true
	}
	defer r.unlockAccount(uid)
	results = r.runAutoAll(a)
	log.Printf("panel: 一键完成可自动任务 uid=%s 共 %d 项", uid, len(results))
	return results, false
}

// RunAutoOne 单任务动作：执行对应动作 → 回读进度 → 汇报结果（map）。
// 返回 (resp, engaged)。engaged=true 表示该账号已有任务动作在跑。
func (r *Runner) RunAutoOne(uid, taskCode string) (map[string]any, bool) {
	a := r.accountByUID(uid)
	if a == nil {
		return nil, false
	}
	act := autoActionFor(taskCode)
	if act == nil {
		return map[string]any{"ok": false, "error": "该任务无对应接口，无法自动完成"}, false
	}
	if !r.tryLockAccount(uid) {
		return nil, true
	}
	defer r.unlockAccount(uid)
	before, err := r.taskByCode(a, act.TaskCode)
	if err != nil {
		return map[string]any{"ok": false, "error": "list tasks: " + err.Error()}, false
	}
	if before == nil {
		return map[string]any{"ok": false, "error": "该账号没有此任务"}, false
	}
	if before.Claimed {
		return map[string]any{"ok": true, "skipped": true, "message": "该任务已领取过奖励"}, false
	}
	msg, err := act.run(r, a)
	if err != nil {
		return map[string]any{"ok": false, "error": "执行失败: " + err.Error()}, false
	}
	after, aerr := r.taskByCodeWaiting(a, act.TaskCode)
	progressBefore, progressAfter := taskProgressText(before), ""
	claimable := false
	if aerr == nil && after != nil {
		progressAfter = taskProgressText(after)
		claimable = after.Claimable
	}
	resp := map[string]any{
		"ok":              true,
		"task_code":       act.TaskCode,
		"message":         msg,
		"progress_before": progressBefore,
		"progress_after":  progressAfter,
		"claimable":       claimable,
		"attempt":         act.Attempt,
	}
	if claimable {
		if credit, energy, cerr := r.up.ClaimReward(a, act.TaskCode); cerr == nil {
			resp["claimed"] = true
			resp["credit"] = credit
			resp["energy"] = energy
			if credit > 0 || energy > 0 {
				resp["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
			} else {
				resp["message"] = msg + "；奖励此前已领取"
			}
		} else {
			resp["claim_error"] = cerr.Error()
			resp["message"] = msg + "；达标但领奖失败，可手动重试"
		}
	}
	return resp, false
}
