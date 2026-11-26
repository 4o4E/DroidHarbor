package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"strings"
)

//go:embed appinput/classes.dex.b64
var appInputDEX string

var appInputImage = strings.NewReplacer("\r", "", "\n", "").Replace(appInputDEX)

// Stream the whole compound command over shell stdin: the task gateway limits
// the ADB service name, and an interrupted script must not start a partial edit.
func appInputScript(observed appForeground, target appNode, text string) string {
	b := target.rectangle()
	values := base64.StdEncoding.EncodeToString([]byte(target.Text + "\n" + text + "\n"))
	window := appShellQuote("Window{" + observed.window + " u" + observed.user + " ")
	return fmt.Sprintf(`(
umask 077
[ "$(getprop ro.build.version.sdk)" -ge 26 ] 2>/dev/null || exit 68
work=$(mktemp -d /data/local/tmp/droidharbor-appinput.XXXXXXXX) || exit 73
trap 'rc=$?; rm -rf -- "$work" || exit 74; exit "$rc"' EXIT
trap 'exit 75' HUP INT TERM
printf %%s %s | base64 -d > "$work/edit.dex" || exit 73
chmod 400 "$work/edit.dex" || exit 73
if ! dumpsys window displays | grep -E '^[[:space:]]*mCurrentFocus[[:space:]]*=' | grep -Fq %s; then
    printf 'NOT_SENT\n'
    exit 0
fi
printf %%s %s | base64 -d | CLASSPATH="$work/edit.dex" timeout 8s app_process /system/bin top.e404.droidharbor.appinput.Main %s %s %s %d %d %d %d
)
`, appInputImage, window, values, appShellQuote(target.Package), appShellQuote(target.Class), appShellQuote(target.ID), b.Min.X, b.Min.Y, b.Max.X, b.Max.Y)
}

func (r *appRun) inputUnicode(ctx context.Context, observed appForeground, target appNode, text string) *commandFailure {
	if strings.ContainsAny(target.Text, "\r\n") {
		return appFail("APP_INPUT_UNSUPPORTED", "Unicode 输入只支持可核验的单行字段，未写入文本", 5)
	}
	out := r.adbScript(ctx, appInputScript(observed, target, text))
	if !out.started {
		if ctx.Err() != nil {
			return appObservationFailure(ctx, ctx.Err())
		}
		return appFail("APP_NOT_SENT", "无法启动当前任务的 Unicode 输入，未写入文本", 4)
	}
	marker := strings.TrimSpace(string(out.stdout))
	if ctx.Err() == nil && out.exitCode == 74 {
		switch marker {
		case "SENT", "REJECTED":
			r.result.Dispatch = "sent"
		case "NOT_SENT", "SELECTION_UNVERIFIED", "UNSUPPORTED":
		default:
			r.result.Dispatch = "delivery_unknown"
		}
		return appFail("APP_INPUT_CLEANUP_FAILED", "未能清理手机上的临时输入组件；先观察当前字段，不得自动重放", 4)
	}
	if ctx.Err() == nil {
		switch out.exitCode {
		case 68:
			return appFail("APP_INPUT_UNSUPPORTED", "当前 Android 版本不支持任务级 Unicode 编辑，未写入文本", 5)
		case 73:
			return appFail("APP_INPUT_PREPARATION_FAILED", "未能在手机上准备临时输入组件，未写入文本", 4)
		}
	}
	if ctx.Err() == nil && out.err == nil {
		switch marker {
		case "SENT":
			r.result.Dispatch = "sent"
			return nil
		case "REJECTED":
			r.result.Dispatch = "sent"
			return appFail("APP_INPUT_REJECTED", "编辑器未接受 Unicode 写入，未能验证字段", 5)
		case "NOT_SENT":
			return appFail("APP_STATE_CHANGED", "Android 未确认窗口、当前字段及原值仍与观察一致，未写入文本", 5)
		case "SELECTION_UNVERIFIED":
			return appFail("APP_SELECTION_UNVERIFIED", "未确认同一字段的现有文本已全部选中，未写入新文本", 5)
		case "UNSUPPORTED":
			return appFail("APP_INPUT_UNSUPPORTED", "当前 Android 或编辑器不支持任务级 Unicode 编辑，未写入文本", 5)
		}
	}
	r.result.Dispatch = "delivery_unknown"
	return appFail("APP_DELIVERY_UNKNOWN", "Unicode 动作可能已送达 Android；先观察字段，不得自动重放", 6)
}
