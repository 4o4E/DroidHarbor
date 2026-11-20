package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	appUsage              = `用法：droidharbor-cli app <open|find|wait|input|tap> --task-dir <目录> --package <包名> [--timeout 12s] [--target <JSON>] [--text <ASCII>] [--expect-package <包名>] [--expect-activity <包名/Activity>] [--expect-node <JSON>]...`
	appDefaultTimeout     = 12 * time.Second
	appMinTimeout         = 2 * time.Second
	appMaxTimeout         = 60 * time.Second
	appScreenshotReserve  = 2500 * time.Millisecond
	appTreeTimeout        = 4500 * time.Millisecond
	appTreeInterval       = 1100 * time.Millisecond
	appForegroundInterval = 200 * time.Millisecond
	appEvidenceLimit      = 6
	appSelectAll           = "input keycombination -t 50 KEYCODE_CTRL_LEFT KEYCODE_A"
)

// Stock Android XML may put the field's placeholder in its text attribute.
// Without an explicit hint flag, a cursor at the end proves the editor
// actually contains text; XML equality alone is insufficient.
func appFieldEquals(node appNode, text string, selectionEnd int) bool {
	return node.Text == text && !node.ShowingHint && (node.HintKnown || selectionEnd == len(text))
}

type appSelector struct {
	ID          string  `json:"id"`
	Class       string  `json:"class"`
	Text        *string `json:"text"`
	Description *string `json:"description"`
	Enabled     *bool   `json:"enabled"`
	Focused     *bool   `json:"focused"`
	Clickable   *bool   `json:"clickable"`
	Selected    *bool   `json:"selected"`
}

func (s *appSelector) String() string { return "控件条件 JSON" }

func (s *appSelector) Set(value string) error {
	dec := json.NewDecoder(strings.NewReader(value))
	dec.DisallowUnknownFields()
	var parsed appSelector
	if err := dec.Decode(&parsed); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF || parsed == (appSelector{}) {
		return errors.New("需要一个非空控件条件对象")
	}
	*s = parsed
	return nil
}

func (s appSelector) matches(n appNode) bool {
	return (s.ID == "" || s.ID == n.ID) && (s.Class == "" || s.Class == n.Class) &&
		(s.Text == nil || *s.Text == n.Text) && (s.Description == nil || *s.Description == n.Description) &&
		(s.Enabled == nil || *s.Enabled == n.Enabled) && (s.Focused == nil || *s.Focused == n.Focused) &&
		(s.Clickable == nil || *s.Clickable == n.Clickable) && (s.Selected == nil || *s.Selected == n.Selected)
}

type appExpectation struct {
	pkg, activity string
	nodes         []appSelector
}

type appOptions struct {
	action, directory, pkg, text string
	target                       appSelector
	expect                       appExpectation
	timeout                      time.Duration
}

func parseAppOptions(args []string) (appOptions, error) {
	var o appOptions
	if len(args) == 0 {
		return o, errors.New("缺少 app 子命令")
	}
	o.action = args[0]
	flags := newCommandFlagSet("app " + o.action)
	flags.StringVar(&o.directory, "task-dir", "", "受保护的任务目录")
	flags.StringVar(&o.pkg, "package", "", "目标应用包名")
	flags.StringVar(&o.text, "text", "", "输入的 ASCII 文本")
	flags.DurationVar(&o.timeout, "timeout", appDefaultTimeout, "端到端截止时间")
	flags.Var(&o.target, "target", "操作目标控件条件 JSON")
	flags.StringVar(&o.expect.pkg, "expect-package", "", "预期前台包名")
	flags.StringVar(&o.expect.activity, "expect-activity", "", "预期前台组件")
	flags.Func("expect-node", "预期控件条件 JSON，可重复", func(value string) error {
		var s appSelector
		if len(o.expect.nodes) == appEvidenceLimit {
			return errors.New("最多指定 6 个预期控件")
		}
		if err := s.Set(value); err != nil {
			return err
		}
		o.expect.nodes = append(o.expect.nodes, s)
		return nil
	})
	if err := flags.Parse(args[1:]); err != nil {
		return o, errors.New("选项或控件条件 JSON 无效")
	}
	if flags.NArg() != 0 || o.directory == "" || !appPackagePattern.MatchString(o.pkg) || o.timeout < appMinTimeout || o.timeout > appMaxTimeout {
		return o, errors.New("需要 --task-dir、有效的 --package，且 --timeout 必须在 2s 至 60s 内")
	}
	target := o.target != (appSelector{})
	expect := o.expect.pkg != "" || o.expect.activity != "" || len(o.expect.nodes) != 0
	switch o.action {
	case "open", "wait":
		if target || o.text != "" {
			return o, errors.New("open 和 wait 不接受 --target 或 --text")
		}
	case "find":
		if !target || expect || o.text != "" {
			return o, errors.New("find 只接受 --target 定位条件，不接受预期条件或 --text")
		}
	case "input":
		if !target || expect || !appInputSupported(o.text) {
			return o, errors.New("input 需要 --target 和非空可打印 ASCII --text（不支持字面 %s），以精确读回作为预期条件")
		}
	case "tap":
		if !target || !expect || o.text != "" {
			return o, errors.New("tap 需要 --target 及预期的前台或控件条件，不接受 --text")
		}
	default:
		return o, errors.New("不支持的 app 子命令")
	}
	if o.expect.activity != "" {
		pkg, component := appComponent(o.expect.activity)
		if pkg == "" || o.expect.pkg != "" && pkg != o.expect.pkg {
			return o, errors.New("--expect-activity 必须为与预期包名一致的 包名/Activity")
		}
		o.expect.pkg, o.expect.activity = pkg, component
	}
	if o.expect.pkg == "" {
		o.expect.pkg = o.pkg
	}
	if !appPackagePattern.MatchString(o.expect.pkg) || o.action == "open" && o.expect.pkg != o.pkg {
		return o, errors.New("预期包名无效；open 的预期包名必须是启动目标")
	}
	return o, nil
}

func appInputSupported(text string) bool {
	if text == "" || len(text) > 256 || strings.Contains(text, "%s") {
		return false
	}
	for i := range len(text) {
		if text[i] < 32 || text[i] > 126 {
			return false
		}
	}
	return true
}

type appMatches struct {
	Count int       `json:"count"`
	Nodes []appNode `json:"nodes,omitempty"`
}

func matchAppNodes(nodes []appNode, s appSelector) appMatches {
	var matches appMatches
	for _, n := range nodes {
		if s.matches(n) {
			matches.Count++
			if len(matches.Nodes) < appEvidenceLimit {
				matches.Nodes = append(matches.Nodes, n)
			}
		}
	}
	return matches
}

type appObservation struct {
	ObservedAt string        `json:"observedAt"`
	Foreground appForeground `json:"foreground"`
	Hierarchy  string        `json:"hierarchy"`
	Matches    *appMatches   `json:"matches,omitempty"`
	Expected   []appMatches  `json:"expected,omitempty"`
	nodes      []appNode
}

type appScreenshot struct {
	Path       string `json:"path"`
	ObservedAt string `json:"observedAt"`
}

type appResult struct {
	Operation          string          `json:"operation"`
	Dispatch           string          `json:"dispatch"`
	Verified           bool            `json:"verified"`
	ForegroundVerified bool            `json:"foregroundVerified,omitempty"`
	PageVerified       *bool           `json:"pageVerified,omitempty"`
	AlreadySatisfied   bool            `json:"alreadySatisfied,omitempty"`
	Launcher           string          `json:"launcher,omitempty"`
	Target             *appNode        `json:"target,omitempty"`
	FieldValue         string          `json:"fieldValue,omitempty"`
	Last               *appObservation `json:"lastObservation,omitempty"`
	FirstScreen        *appScreenshot  `json:"firstScreen,omitempty"`
	Screenshot         *appScreenshot  `json:"screenshot,omitempty"`
	ScreenshotError    string          `json:"screenshotError,omitempty"`
	ElapsedMillis      int64           `json:"elapsedMs"`
	Observations       int             `json:"observations"`
}

type appRun struct {
	options   appOptions
	adb       appADB
	directory string
	result    appResult
}

func appCommand(args []string) (any, *commandFailure) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		return map[string]string{"usage": appUsage}, nil
	}
	options, err := parseAppOptions(args)
	if err != nil {
		return nil, &commandFailure{Code: "INVALID_ARGUMENT", Message: err.Error() + "；" + appUsage, exitCode: 2}
	}
	start := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(options.timeout))
	defer cancel()
	run := appRun{options: options}
	run.result = appResult{Operation: options.action, Dispatch: "not_sent"}
	directory, state, env, failure := activeADBConnection(ctx, options.directory)
	if failure == nil {
		run.directory, run.adb = directory, taskAppADB(state, env)
		failure = run.execute(ctx)
	} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		failure = appFail("APP_TIMEOUT", "截止时间内未核验活动任务连接，动作未发出", 5)
	}
	run.result.ElapsedMillis = time.Since(start).Milliseconds()
	if failure != nil {
		failure.Details = &run.result
		return nil, failure
	}
	return &run.result, nil
}

func appFail(code, message string, exit int) *commandFailure {
	return &commandFailure{Code: code, Message: message, exitCode: exit}
}

func (r *appRun) command(ctx context.Context, script string) appExecResult {
	return r.adb(ctx, appProbeLimit, "shell", "-T", script)
}

func (r *appRun) foreground(ctx context.Context) (appForeground, error) {
	out := r.command(ctx, "dumpsys window displays")
	if err := appProbeError("未能读取前台窗口", out); err != nil {
		return appForeground{}, err
	}
	return parseAppForeground(out.stdout)
}

func (r *appRun) observe(ctx context.Context, pkg string, tree bool) (*appObservation, error) {
	r.result.Observations++
	front, err := r.foreground(ctx)
	if err != nil {
		return nil, err
	}
	obs := &appObservation{ObservedAt: appStamp(), Foreground: front, Hierarchy: "not_requested"}
	if !tree || front.Package != pkg {
		return obs, nil
	}
	probe, cancel := context.WithTimeout(ctx, appTreeTimeout)
	treeOut := r.command(probe, appDumpScript)
	cancel()
	obs.Hierarchy = "unavailable"
	if err := appProbeError("未能获取可靠界面树", treeOut); err != nil {
		return obs, err
	}
	if bytes.Contains(treeOut.stderr, []byte("ERROR:")) {
		return obs, errors.New("Android 未能导出当前控件树")
	}
	nodes, err := parseAppNodes(treeOut.stdout, pkg)
	if err != nil || len(nodes) == 0 {
		return obs, errors.New("界面树不完整，或没有属于当前应用的可见节点")
	}
	after, err := r.foreground(ctx)
	if err != nil {
		return obs, err
	}
	obs.Foreground, obs.ObservedAt = after, appStamp()
	if after != front {
		return obs, errors.New("取控件树期间前台窗口发生变化")
	}
	obs.Hierarchy, obs.nodes = "observed", nodes
	return obs, nil
}

func (r *appRun) observeTarget(ctx context.Context) (*appObservation, appMatches, error) {
	obs, err := r.observe(ctx, r.options.pkg, true)
	var matches appMatches
	if obs != nil {
		r.result.Last = obs
		if obs.Hierarchy == "observed" {
			matches = matchAppNodes(obs.nodes, r.options.target)
			obs.Matches = &matches
		}
	}
	return obs, matches, err
}

func (e appExpectation) satisfied(obs *appObservation) bool {
	if obs.Foreground.Package != e.pkg || e.activity != "" && obs.Foreground.Component != e.activity {
		return false
	}
	if len(e.nodes) != 0 && obs.Hierarchy != "observed" {
		return false
	}
	ok := true
	for _, selector := range e.nodes {
		matches := matchAppNodes(obs.nodes, selector)
		obs.Expected = append(obs.Expected, matches)
		ok = ok && matches.Count != 0
	}
	return ok
}

func (r *appRun) screenshot(ctx context.Context) (*appScreenshot, error) {
	shotCtx, cancel := context.WithTimeout(ctx, appScreenshotReserve)
	defer cancel()
	out := r.adb(shotCtx, appPNGLimit, "exec-out", "screencap", "-p")
	if err := appProbeError("未能读取当前截图", out); err != nil {
		return nil, err
	}
	path, err := saveAppPNG(r.directory, out.stdout)
	if err != nil {
		return nil, err
	}
	return &appScreenshot{Path: path, ObservedAt: appStamp()}, nil
}

func (r *appRun) failedObservation(root context.Context, failure *commandFailure) *commandFailure {
	shot, err := r.screenshot(root)
	if err != nil {
		r.result.ScreenshotError = err.Error()
	} else {
		r.result.Screenshot = shot
	}
	return failure
}

func appObservationFailure(work context.Context, err error) *commandFailure {
	if errors.Is(work.Err(), context.DeadlineExceeded) {
		return appFail("APP_TIMEOUT", "在截止时间内未验证预期状态", 5)
	}
	return appFail("APP_EVIDENCE_UNAVAILABLE", "未能自动验证："+err.Error(), 5)
}

// Polling never owns a mutating operation. A timeout leaves the last complete
// evidence in place even when a new probe was interrupted mid-read.
func (r *appRun) wait(ctx context.Context, pkg string, tree bool, check func(*appObservation) bool) *commandFailure {
	for ctx.Err() == nil {
		obs, err := r.observe(ctx, pkg, tree)
		if obs != nil && (err == nil || ctx.Err() == nil || r.result.Last == nil) {
			r.result.Last = obs
		}
		if err != nil {
			return appObservationFailure(ctx, err)
		}
		if ctx.Err() == nil && check(obs) {
			return nil
		}
		interval := appForegroundInterval
		if tree && obs.Foreground.Package == pkg {
			interval = appTreeInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return appFail("APP_TIMEOUT", "在截止时间内未验证预期状态", 5)
}

func (r *appRun) dispatch(ctx context.Context, script string) *commandFailure {
	out := r.command(ctx, script)
	if !out.started {
		if ctx.Err() != nil {
			return appFail("APP_TIMEOUT", "已到动作派发截止时间，动作未发出", 5)
		}
		return appFail("APP_NOT_SENT", "无法启动当前任务的原生 ADB 动作", 4)
	}
	if out.err != nil || bytes.Contains(out.stderr, []byte("Error:")) || bytes.Contains(out.stdout, []byte("Error:")) {
		r.result.Dispatch = "delivery_unknown"
		return appFail("APP_DELIVERY_UNKNOWN", "原生动作可能已送达 Android；不得自动重放", 6)
	}
	r.result.Dispatch = "sent"
	return nil
}

func (r *appRun) execute(root context.Context) *commandFailure {
	deadline, _ := root.Deadline()
	reserve := min(appScreenshotReserve, r.options.timeout/3)
	work, cancel := context.WithDeadline(root, deadline.Add(-reserve))
	defer cancel()
	var failure *commandFailure
	switch r.options.action {
	case "open":
		failure = r.open(root, work)
	case "wait":
		e := r.options.expect
		failure = r.wait(work, e.pkg, len(e.nodes) != 0, e.satisfied)
	default:
		failure = r.withTarget(work)
	}
	if failure != nil {
		if failure.Code == "APP_TARGET_AMBIGUOUS" || failure.Code == "APP_EXPECTATION_ALREADY_MET" || failure.Code == "APP_SCREENSHOT_UNAVAILABLE" {
			return failure
		}
		return r.failedObservation(root, failure)
	}
	r.result.Verified = true
	return nil
}

func (r *appRun) open(root, work context.Context) *commandFailure {
	const launcher = " --user current -a android.intent.action.MAIN -c android.intent.category.LAUNCHER"
	out := r.command(work, "cmd package resolve-activity --brief"+launcher+" -p "+appShellQuote(r.options.pkg))
	if err := appProbeError("未能解析应用启动入口", out); err != nil {
		return appObservationFailure(work, err)
	}
	pkg, component := "", ""
	for line := range bytes.Lines(out.stdout) {
		if p, c := appComponent(string(bytes.TrimSpace(line))); p != "" {
			if component != "" && component != c {
				return appFail("APP_LAUNCHER_UNAVAILABLE", "启动入口解析结果不唯一；未启动应用", 5)
			}
			pkg, component = p, c
		}
	}
	if pkg != r.options.pkg {
		return appFail("APP_LAUNCHER_UNAVAILABLE", "目标当前用户没有可验证的启动入口；可能未安装或未启用", 5)
	}
	r.result.Launcher = component
	if failure := r.dispatch(work, "am start"+launcher+" -n "+appShellQuote(component)); failure != nil {
		return failure
	}
	foreground := appExpectation{pkg: r.options.pkg}
	if failure := r.wait(work, foreground.pkg, false, foreground.satisfied); failure != nil {
		return failure
	}
	r.result.ForegroundVerified = true
	var err error
	r.result.FirstScreen, err = r.screenshot(root)
	if err != nil {
		r.result.ScreenshotError = err.Error()
	}
	e := r.options.expect
	if e.activity != "" || len(e.nodes) != 0 {
		verified := false
		r.result.PageVerified = &verified
		if failure := r.wait(work, e.pkg, len(e.nodes) != 0, e.satisfied); failure != nil {
			return failure
		}
		verified = true
	}
	if err != nil {
		return appFail("APP_SCREENSHOT_UNAVAILABLE", "前台已验证，但未能取得首屏截图", 5)
	}
	return nil
}

func (r *appRun) withTarget(work context.Context) *commandFailure {
	obs, matches, err := r.observeTarget(work)
	if err != nil {
		return appObservationFailure(work, err)
	}
	if obs.Foreground.Package != r.options.pkg {
		return appFail("APP_NOT_FOREGROUND", "目标应用当前未获得窗口焦点，动作未发出", 5)
	}
	switch {
	case matches.Count == 0:
		return appFail("APP_TARGET_NOT_FOUND", "当前界面树未匹配到目标；未能自动验证", 5)
	case matches.Count > 1:
		return appFail("APP_TARGET_AMBIGUOUS", "目标匹配到多个候选，动作未发出", 5)
	}
	if r.options.action == "find" {
		return nil
	}
	target := matches.Nodes[0]
	r.result.Target = &target
	if r.options.action == "input" {
		return r.input(work, obs, target)
	}
	return r.tap(work, obs, target)
}

func (r *appRun) tap(work context.Context, obs *appObservation, target appNode) *commandFailure {
	x, y, ok := target.center()
	if !target.Enabled || !target.Clickable || !ok {
		return appFail("APP_NOT_CLICKABLE", "目标未暴露可用的点击区域，动作未发出", 5)
	}
	e := r.options.expect
	if e.satisfied(obs) {
		return appFail("APP_EXPECTATION_ALREADY_MET", "操作前已满足预期条件，不能据此验证本次操作；动作未发出", 5)
	}
	if failure := r.dispatch(work, "input tap "+strconv.Itoa(x)+" "+strconv.Itoa(y)); failure != nil {
		return failure
	}
	return r.wait(work, e.pkg, len(e.nodes) != 0, e.satisfied)
}

func (r *appRun) input(work context.Context, obs *appObservation, target appNode) *commandFailure {
	if _, _, visible := target.center(); !visible || !target.editField() {
		return appFail("APP_NOT_EDITABLE", "目标不是已聚焦且可编辑的普通文本字段，动作未发出", 5)
	}
	editor := r.command(work, "dumpsys input_method")
	if err := appProbeError("未能核对当前文本编辑器", editor); err != nil {
		return appObservationFailure(work, err)
	}
	before := appEditor(editor.stdout, target.Package)
	if before.identity == "" {
		return appFail("APP_NOT_EDITABLE", "Android 未确认当前应用有活动文本编辑器，动作未发出", 5)
	}
	text := r.options.text
	if appFieldEquals(target, text, max(before.start, before.end)) {
		r.result.FieldValue = target.Text
		r.result.AlreadySatisfied = true
		return nil
	}
	if target.Text != "" && !target.ShowingHint && !before.selects(target.Text) {
		help := r.command(work, "input help")
		if work.Err() != nil {
			return appObservationFailure(work, work.Err())
		}
		if !bytes.Contains(help.stdout, []byte("keycombination")) && !bytes.Contains(help.stderr, []byte("keycombination")) {
			return appFail("APP_REPLACE_UNSUPPORTED", "当前 Android 原生输入不支持一次选择并替换现有字段；未编辑", 5)
		}
		if failure := r.selectForReplacement(work, target, before); failure != nil {
			return failure
		}
	}
	var script strings.Builder
	// One remote dispatch, each key in order. A single Android input-text
	// batch can lose the intended ordering in an asynchronous app editor.
	for i := range len(text) {
		if i > 0 {
			script.WriteString(" && ")
		}
		script.WriteString("input text ")
		value := text[i : i+1]
		if value == " " {
			value = "%s"
		}
		script.WriteString(appShellQuote(value))
	}
	front, err := r.foreground(work)
	if err != nil {
		return appObservationFailure(work, err)
	}
	if front != obs.Foreground {
		return appFail("APP_STATE_CHANGED", "派发输入前窗口已改变，动作未发出", 5)
	}
	if failure := r.dispatch(work, script.String()); failure != nil {
		return failure
	}
	focused := true
	scope := appSelector{ID: target.ID, Class: target.Class, Focused: &focused}
	return r.wait(work, r.options.pkg, true, func(next *appObservation) bool {
		if next.Foreground != obs.Foreground || next.Hierarchy != "observed" {
			return false
		}
		// The same focused field may resize when its clear button appears.
		// Verify its served editor identity rather than pinning screen pixels.
		fields := matchAppNodes(next.nodes, scope)
		next.Matches = &fields
		if fields.Count != 1 || !fields.Nodes[0].editField() || fields.Nodes[0].Text != text {
			return false
		}
		field := fields.Nodes[0]
		if field.ShowingHint {
			return false
		}
		editor := r.command(work, "dumpsys input_method")
		current := appEditor(editor.stdout, field.Package)
		if editor.err != nil || current.identity != before.identity || !appFieldEquals(field, text, max(current.start, current.end)) || work.Err() != nil {
			return false
		}
		r.result.FieldValue = field.Text
		return true
	})
}

func (s appEditState) selects(text string) bool {
	units := 0
	for _, r := range text {
		units += utf16.RuneLen(r)
	}
	return min(s.start, s.end) == 0 && max(s.start, s.end) == units
}

func (r *appRun) selectForReplacement(work context.Context, target appNode, before appEditState) *commandFailure {
	selection := r.command(work, appSelectAll)
	if err := appProbeError("未能选择现有字段；未写入新文本", selection); err != nil {
		return appObservationFailure(work, err)
	}
	ctx, cancel := context.WithTimeout(work, 750*time.Millisecond)
	defer cancel()
	for ctx.Err() == nil {
		probe := r.command(ctx, "dumpsys input_method")
		selected := appEditor(probe.stdout, target.Package)
		if probe.err != nil || selected.identity != before.identity {
			break
		}
		if selected.selects(target.Text) {
			return nil
		}
		timer := time.NewTimer(65 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	if work.Err() != nil {
		return appObservationFailure(work, work.Err())
	}
	return appFail("APP_SELECTION_UNVERIFIED", "未确认同一字段的现有文本已全部选中；未写入新文本", 5)
}
