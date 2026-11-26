package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	fixtureApp    = "com.example.catalog"
	fixtureFront  = "Display: mDisplayId=0\n  mCurrentFocus=Window{bbf8139 u0 com.example.catalog/.MainActivity}\n"
	fixtureEditor = `Input method client state for android.view.inputmethod.InputMethodManager@1022:
  mActive=true mRestartOnNextWindowFocus=false
  mServedConnecting=false
  mCurrentEditorInfo:
    inputType=0x1 imeOptions=0x3 privateImeOptions=null
    packageName=com.example.catalog fieldId=2131165130 fieldName=null
  mServedInputConnection=RemoteInputConnectionImpl{native}
  mCursorSelStart=7 mCursorSelEnd=7 mCursorCandStart=-1 mCursorCandEnd=-1`
	fixtureSearch = `<node package="com.example.catalog" class="android.widget.Button" text="Search" clickable="true" enabled="true" bounds="[10,10][70,30]"/>`
	fixtureMarket = `<node package="com.example.catalog" class="android.widget.TextView" content-desc="Market RTX5090" enabled="true" bounds="[10,100][250,140]"/>`
)

func fixtureTree(nodes string) string {
	return `<hierarchy rotation="0">` + nodes + `</hierarchy>`
}

func fixtureField(value string) string {
	return `<node package="com.example.catalog" class="android.widget.EditText" resource-id="com.example.catalog:id/query" text="` + value + `" focused="true" enabled="true" bounds="[10,30][230,70]"/>`
}

func fixturePNG(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 3, 3))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type appFixture struct {
	t               *testing.T
	before, after   string
	front           string
	mutations       []string
	dumps, shots    int
	failDispatch    bool
	selectionAtZero bool
	selected        bool
	png             []byte
	script          appADBScript
	editor          func(after bool) string
}

func (f *appFixture) adb(ctx context.Context, _ int, args ...string) appExecResult {
	if err := ctx.Err(); err != nil {
		return appExecResult{err: err}
	}
	ok := func(s string) appExecResult { return appExecResult{started: true, stdout: []byte(s)} }
	if len(args) == 3 && args[0] == "exec-out" && args[1] == "screencap" && args[2] == "-p" {
		f.shots++
		return appExecResult{started: true, stdout: f.png}
	}
	if len(args) != 3 || args[0] != "shell" || args[1] != "-T" {
		f.t.Fatalf("unexpected adb argv %q", args)
	}
	script := args[2]
	switch {
	case script == "dumpsys window displays":
		return ok(f.front)
	case script == appDumpScript:
		f.dumps++
		if len(f.mutations) == 0 {
			return ok(f.before)
		}
		return ok(f.after)
	case script == "dumpsys input_method":
		if f.editor != nil {
			return ok(f.editor(len(f.mutations) != 0))
		}
		if f.selectionAtZero {
			return ok(strings.Replace(fixtureEditor, "mCursorSelStart=7 mCursorSelEnd=7", "mCursorSelStart=0 mCursorSelEnd=0", 1))
		}
		if f.selected && len(f.mutations) == 0 {
			return ok(strings.Replace(fixtureEditor, "mCursorSelStart=7", "mCursorSelStart=0", 1))
		}
		return ok(fixtureEditor)
	case script == "input help":
		return ok("keycombination [-t duration(ms)] <key code 1> <key code 2> ...")
	case script == appSelectAll:
		if f.selected {
			f.t.Fatal("selection repeated")
		}
		f.selected = true
		return ok("")
	case strings.HasPrefix(script, "cmd package resolve-activity --brief"):
		return ok("priority=0 preferredOrder=0 match=0x108000\ncom.example.catalog/.LauncherAlias\n")
	case strings.HasPrefix(script, "input ") || strings.HasPrefix(script, "am start "):
		f.mutations = append(f.mutations, script)
		if f.failDispatch {
			return appExecResult{started: true, err: errors.New("fixture transport closed")}
		}
		return ok("")
	default:
		f.t.Fatalf("unexpected Android command %q", script)
		return appExecResult{}
	}
}

func runFixtureApp(t *testing.T, f *appFixture, budget time.Duration, action string, flags ...string) (appResult, *commandFailure) {
	t.Helper()
	dir := t.TempDir()
	args := append([]string{action, "--package", fixtureApp, "--task-dir", dir}, flags...)
	o, err := parseAppOptions(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	f.t, f.front, f.png = t, fixtureFront, fixturePNG(t)
	r := appRun{options: o, adb: f.adb, adbScript: f.script, directory: dir, result: appResult{Operation: action, Dispatch: "not_sent"}}
	failure := r.execute(ctx)
	for _, s := range []*appScreenshot{r.result.Screenshot, r.result.FirstScreen} {
		if s != nil {
			data, err := os.ReadFile(s.Path)
			if err != nil || filepath.Dir(s.Path) != dir || !bytes.Equal(data, f.png) {
				t.Fatalf("screenshot did not contain the actual private evidence: %v", err)
			}
		}
	}
	return r.result, failure
}

func TestAppInputVerifiesOnlySameField(t *testing.T) {
	for _, tc := range []struct {
		name, before, actual, failureCode string
		placeholder                       bool
	}{
		{"exact replacement", "RTX5080", "RTX5090", "", false},
		{"query elsewhere does not verify old field", "RTX5080", "RTX5080RTX5090", "APP_TIMEOUT", false},
		{"matching placeholder is not the actual value", "RTX5090", "RTX5090", "APP_SELECTION_UNVERIFIED", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := strings.Replace(fixtureField(tc.actual), "[230,70]", "[211,70]", 1)
			f := &appFixture{before: fixtureTree(fixtureField(tc.before)), after: fixtureTree(after + fixtureMarket), selectionAtZero: tc.placeholder}
			budget := appScreenshotReserve + appTreeInterval/2
			if tc.placeholder {
				budget = 4 * time.Second
			}
			r, failure := runFixtureApp(t, f, budget, "input", "--target", `{"id":"com.example.catalog:id/query"}`, "--text", "RTX5090")
			if tc.failureCode == "" && (failure != nil || !r.Verified || r.FieldValue != tc.actual) || tc.failureCode != "" && (failure == nil || failure.Code != tc.failureCode || r.Verified || r.Screenshot == nil || r.FieldValue != "") {
				t.Fatalf("result %+v failure %+v", r, failure)
			}
			if tc.placeholder {
				if r.Dispatch != "not_sent" || len(f.mutations) != 0 {
					t.Fatalf("unproven replacement sent text: %+v, %q", r, f.mutations)
				}
				return
			}
			want := "input text 'R' && input text 'T' && input text 'X' && input text '5' && input text '0' && input text '9' && input text '0'"
			if r.Dispatch != "sent" || r.Last.Matches.Nodes[0].Text != tc.actual || len(f.mutations) != 1 || f.mutations[0] != want {
				t.Fatalf("wrong field proof or repeated edit: %+v actions %q", r.Last.Matches, f.mutations)
			}
		})
	}
}

func TestAppTapStopsWithoutReplaying(t *testing.T) {
	for _, tc := range []struct {
		name, before, after, code, dispatch string
		lost                                bool
		count                               int
	}{
		{"new page marker", fixtureSearch, fixtureMarket, "", "sent", false, 1},
		{"duplicated target", fixtureSearch + fixtureSearch, "", "APP_TARGET_AMBIGUOUS", "not_sent", false, 0},
		{"preexisting marker", fixtureSearch + fixtureMarket, "", "APP_EXPECTATION_ALREADY_MET", "not_sent", false, 0},
		{"missing marker", fixtureSearch, fixtureSearch, "APP_TIMEOUT", "sent", false, 1},
		{"unknown delivery", fixtureSearch, fixtureMarket, "APP_DELIVERY_UNKNOWN", "delivery_unknown", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &appFixture{before: fixtureTree(tc.before), after: fixtureTree(tc.after), failDispatch: tc.lost}
			r, failure := runFixtureApp(t, f, appScreenshotReserve+appTreeInterval/2, "tap", "--target", `{"text":"Search"}`, "--expect-node", `{"description":"Market RTX5090"}`)
			if tc.code == "" && (failure != nil || !r.Verified) || tc.code != "" && (failure == nil || failure.Code != tc.code || r.Verified) {
				t.Fatalf("result %+v failure %+v", r, failure)
			}
			if r.Dispatch != tc.dispatch || len(f.mutations) != tc.count {
				t.Fatalf("dispatch %s actions %q", r.Dispatch, f.mutations)
			}
			if tc.code == "APP_TIMEOUT" && (r.Last.Expected[0].Count != 0 || r.Last.Foreground.Package != fixtureApp || f.shots != 1) {
				t.Fatalf("missing timeout observation: %+v", r.Last)
			}
		})
	}
}

func TestAppOpenExposesForegroundWithoutClaimingPage(t *testing.T) {
	f := &appFixture{after: fixtureTree(fixtureSearch)}
	r, failure := runFixtureApp(t, f, appScreenshotReserve+appTreeInterval/2, "open", "--expect-node", `{"description":"Market RTX5090"}`)
	if failure == nil || failure.Code != "APP_TIMEOUT" || !r.ForegroundVerified || r.PageVerified == nil || *r.PageVerified || r.FirstScreen == nil {
		t.Fatalf("page and foreground conflated: %+v failure %+v", r, failure)
	}
	if len(f.mutations) != 1 || !strings.HasSuffix(f.mutations[0], "-n 'com.example.catalog/com.example.catalog.LauncherAlias'") || r.Last.Foreground.Component != "com.example.catalog/com.example.catalog.MainActivity" {
		t.Fatalf("resolver or actual foreground ignored: %q %+v", f.mutations, r.Last)
	}
}

func TestAppFindUnavailableTreeReturnsCurrentScreenshot(t *testing.T) {
	f := &appFixture{before: `<hierarchy><node`}
	r, failure := runFixtureApp(t, f, 3*time.Second, "find", "--target", `{"text":"行情"}`)
	if failure == nil || failure.Code != "APP_EVIDENCE_UNAVAILABLE" || r.Verified || r.Last.Hierarchy != "unavailable" || r.Screenshot == nil || f.dumps != 1 || len(f.mutations) != 0 {
		t.Fatalf("unavailable tree misreported: result %+v failure %+v", r, failure)
	}
}
