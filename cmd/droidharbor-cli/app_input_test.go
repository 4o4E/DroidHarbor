package main

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestAppUnicodeInputRequiresExactReadback(t *testing.T) {
	const text = "红米 K 🍎"
	for _, tc := range []struct {
		name, actual, marker, code, dispatch string
		transportError, changedView          bool
		exitCode                             int
	}{
		{"exact mixed script after IME reconnect", text, "SENT", "", "sent", false, false, 0},
		{"Android ACK does not prove readback", "红米 K", "SENT", "APP_TIMEOUT", "sent", false, false, 0},
		{"different view using same resource ID", text, "SENT", "APP_TIMEOUT", "sent", false, true, 0},
		{"editor changed on device", "旧的搜索词", "NOT_SENT", "APP_STATE_CHANGED", "not_sent", false, false, 0},
		{"refused selection", "旧的搜索词", "SELECTION_UNVERIFIED", "APP_SELECTION_UNVERIFIED", "not_sent", false, false, 0},
		{"unsupported editor", "旧的搜索词", "UNSUPPORTED", "APP_INPUT_UNSUPPORTED", "not_sent", false, false, 0},
		{"temporary helper unavailable", "旧的搜索词", "", "APP_INPUT_PREPARATION_FAILED", "not_sent", false, false, 73},
		{"cleanup failed after Android ACK", text, "SENT", "APP_INPUT_CLEANUP_FAILED", "sent", false, false, 74},
		{"dropped connection after dispatch", text, "SENT", "APP_DELIVERY_UNKNOWN", "delivery_unknown", true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const old = "旧的搜索词"
			f := &appFixture{before: fixtureTree(fixtureField(old)), after: fixtureTree(fixtureField(tc.actual))}
			f.editor = func(after bool) string {
				editor := strings.Replace(fixtureEditor, "  mServedConnecting=false", "  mServedView=android.widget.EditText{aa19 View}\n  mServedConnecting=false", 1)
				if after {
					editor = strings.Replace(editor, "{native}", "{restarted}", 1)
					if tc.changedView {
						editor = strings.Replace(editor, "{aa19 ", "{bb20 ", 1)
					}
				}
				return editor
			}
			f.script = func(_ context.Context, script string) appExecResult {
				payload := base64.StdEncoding.EncodeToString([]byte(old + "\n" + text + "\n"))
				if !strings.Contains(script, payload) || strings.Contains(script, text) {
					t.Fatal("edit did not preserve the two UTF-8 strings without copying them into shell syntax")
				}
				f.mutations = append(f.mutations, "android editor")
				out := appExecResult{started: true, stdout: []byte(tc.marker + "\n"), exitCode: tc.exitCode}
				if tc.transportError || tc.exitCode != 0 {
					out.err = errors.New("ADB reported an error")
				}
				return out
			}
			r, failure := runFixtureApp(t, f, appScreenshotReserve+appTreeInterval/2, "input", "--target", `{"id":"com.example.catalog:id/query"}`, "--text", text)
			if r.Dispatch != tc.dispatch || len(f.mutations) != 1 || f.mutations[0] != "android editor" {
				t.Fatalf("unexpected dispatch or repeat: %+v, %q", r, f.mutations)
			}
			switch {
			case tc.code == "":
				if failure != nil || !r.Verified || r.FieldValue != text {
					t.Fatalf("exact field was not verified: %+v, %+v", r, failure)
				}
			case failure == nil || failure.Code != tc.code || r.Verified || r.FieldValue != "":
				t.Fatalf("unexpected failure or false verification: %+v, %+v", r, failure)
			}
		})
	}
}

func TestAppUnicodeInputRejectsUnsafeTarget(t *testing.T) {
	for _, tc := range []struct {
		name, nodes, code string
	}{
		{"no focus", strings.Replace(fixtureField(""), `focused="true"`, `focused="false"`, 1), "APP_NOT_EDITABLE"},
		{"duplicate selector", fixtureField("") + fixtureField(""), "APP_TARGET_AMBIGUOUS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &appFixture{before: fixtureTree(tc.nodes), script: func(context.Context, string) appExecResult {
				t.Fatal("an unverified target must receive no edit script")
				return appExecResult{}
			}}
			r, failure := runFixtureApp(t, f, appDefaultTimeout, "input", "--target", `{"id":"com.example.catalog:id/query"}`, "--text", "小米 15")
			if failure == nil || failure.Code != tc.code || r.Dispatch != "not_sent" || r.Verified {
				t.Fatalf("unexpected target result: %+v, %+v", r, failure)
			}
		})
	}
}
