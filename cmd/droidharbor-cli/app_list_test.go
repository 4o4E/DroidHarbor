package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func listFixtureRow(top, bottom int, desc string) string {
	return fmt.Sprintf(`<node package="%[1]s" class="fixture.Row" bounds="[10,%[2]d][490,%[3]d]"><node package="%[1]s" class="android.view.View" content-desc="%[4]s" bounds="[10,%[2]d][490,%[3]d]"/></node>`, fixtureApp, top, bottom, desc)
}

func listFixtureViewport(rows, footer, query string) string {
	return fixtureTree(fmt.Sprintf(`<node package="%[1]s" class="android.widget.TextView" text="%[4]s" bounds="[0,10][60,90]"/><node package="%[1]s" class="fixture.List" enabled="true" scrollable="true" bounds="[0,100][500,500]">%[2]s%[3]s</node><node package="%[1]s" class="android.widget.TextView" text="外部末尾" bounds="[0,460][500,480]"/>`, fixtureApp, rows, footer, query))
}

func listFixtureEnd(top int, text string) string {
	return fmt.Sprintf(`<node package="%s" class="android.widget.TextView" text="%s" bounds="[20,%d][300,%d]"/>`, fixtureApp, text, top, top+15)
}

func listFixtureRun(t *testing.T, pages []string, errSend error, extra ...string) (appResult, *commandFailure, int) {
	t.Helper()
	args := []string{"read-list", "--package", fixtureApp, "--task-dir", t.TempDir(), "--target", `{"class":"fixture.List","scrollable":true}`, "--item", `{"class":"fixture.Row"}`, "--expect-node", `{"text":"RTX"}`}
	o, err := parseAppOptions(append(args, extra...))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	var sent int
	png := fixturePNG(t)
	r := appRun{options: o, directory: o.directory, result: appResult{Operation: o.action, Dispatch: "not_sent"}}
	r.adb = func(_ context.Context, _ int, args ...string) appExecResult {
		ok := func(data []byte) appExecResult { return appExecResult{started: true, stdout: data} }
		switch args[len(args)-1] {
		case "dumpsys window displays":
			return ok([]byte(fixtureFront))
		case appDumpScript:
			if sent >= len(pages) {
				t.Fatalf("unexpected observation after swipe %d", sent)
			}
			return ok([]byte(pages[sent]))
		case "-p":
			return ok(png)
		default:
			if args[len(args)-1] != "input swipe 250 380 250 260 900" {
				t.Fatalf("unexpected device command %q", args)
			}
			sent++
			return appExecResult{started: true, err: errSend}
		}
	}
	failure := r.execute(ctx)
	return r.result, failure, sent
}

func listDescriptions(list *appListResult) []string {
	var got []string
	for _, row := range list.Items {
		for _, label := range row.Labels {
			got = append(got, label.Description)
		}
	}
	return got
}

func TestAppReadListVerifiedOverlapKeepsRepeatedItems(t *testing.T) {
	one := listFixtureViewport(listFixtureRow(100, 140, "部分可见")+listFixtureRow(150, 200, "同名商品")+listFixtureRow(260, 320, "锚点商品"), "", "RTX")
	two := listFixtureViewport(listFixtureRow(120, 180, "锚点商品")+listFixtureRow(210, 290, "同名商品")+listFixtureRow(310, 390, "新商品"), listFixtureEnd(430, "列表末尾"), "RTX")
	r, failure, sent := listFixtureRun(t, []string{one, two}, nil, "--end-node", `{"text":"列表末尾"}`)
	want := []string{"同名商品", "锚点商品", "同名商品", "新商品"}
	if failure != nil || sent != 1 || !r.Verified || !r.List.ReachedEnd || r.List.StopReason != "end_reached" || !slices.Equal(listDescriptions(r.List), want) {
		t.Fatalf("result=%+v items=%q failure=%+v swipes=%d", r.List, listDescriptions(r.List), failure, sent)
	}
}

func TestAppReadListStopsBeforeLosingContinuity(t *testing.T) {
	one := listFixtureViewport(listFixtureRow(150, 210, "一")+listFixtureRow(240, 320, "二"), listFixtureEnd(110, "列表末尾"), "RTX")
	different := listFixtureViewport(listFixtureRow(150, 210, "未知的后续"), listFixtureEnd(430, "列表末尾"), "RTX")
	for _, tc := range []struct {
		name, next, reason, code, dispatch string
		send                               error
		flags                              []string
		wantSwipes                         int
	}{
		{"no overlap cannot confirm footer", different, "overlap_missing", "", "sent", nil, []string{"--end-node", `{"text":"列表末尾"}`}, 1},
		{"same viewport is not an end", one, "no_progress", "", "sent", nil, []string{"--end-node", `{"text":"外部末尾"}`}, 1},
		{"same activity different search", strings.Replace(different, `text="RTX"`, `text="other query"`, 1), "page_changed", "APP_LIST_PAGE_CHANGED", "sent", nil, nil, 1},
		{"unknown delivery not replayed", different, "swipe_failed", "APP_DELIVERY_UNKNOWN", "delivery_unknown", errors.New("link lost"), nil, 1},
		{"single screen not a mutation", different, "screen_limit", "", "not_sent", nil, []string{"--max-screens", "1"}, 0},
		{"no swipe without follow-up budget", different, "deadline", "", "not_sent", nil, []string{"--timeout", "2s"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, failure, sent := listFixtureRun(t, []string{one, tc.next}, tc.send, tc.flags...)
			var code string
			if failure != nil {
				code = failure.Code
			}
			if code != tc.code || r.Dispatch != tc.dispatch || sent != tc.wantSwipes || r.List.ReachedEnd || r.List.StopReason != tc.reason || !slices.Equal(listDescriptions(r.List), []string{"一", "二"}) {
				t.Fatalf("result=%+v dispatch=%s items=%q failure=%+v swipes=%d", r.List, r.Dispatch, listDescriptions(r.List), failure, sent)
			}
		})
	}
}

func TestAppReadListRejectsWrongInitialPage(t *testing.T) {
	page := listFixtureViewport(listFixtureRow(150, 200, "其他搜索结果"), "", "other query")
	r, failure, sent := listFixtureRun(t, []string{page}, nil)
	if failure == nil || failure.Code != "APP_LIST_PAGE_CHANGED" || r.List.StopReason != "page_changed" ||
		sent != 0 || r.Dispatch != "not_sent" || len(r.List.Items) != 0 {
		t.Fatalf("result=%+v dispatch=%s failure=%+v swipes=%d", r.List, r.Dispatch, failure, sent)
	}
}

func TestAppReadListStopsOnAmbiguousIdenticalRows(t *testing.T) {
	one := listFixtureViewport(listFixtureRow(120, 160, "起点")+listFixtureRow(260, 300, "同名")+listFixtureRow(340, 400, "同名"), "", "RTX")
	two := listFixtureViewport(listFixtureRow(130, 180, "同名")+listFixtureRow(200, 240, "同名")+listFixtureRow(300, 350, "后续"), "", "RTX")
	r, failure, sent := listFixtureRun(t, []string{one, two}, nil)
	if failure != nil || sent != 1 || r.List.StopReason != "overlap_ambiguous" ||
		!slices.Equal(listDescriptions(r.List), []string{"起点", "同名", "同名"}) {
		t.Fatalf("result=%+v items=%q failure=%+v swipes=%d", r.List, listDescriptions(r.List), failure, sent)
	}
}

func TestAppReadListRejectsUnverifiedViewport(t *testing.T) {
	valid := listFixtureViewport(listFixtureRow(150, 200, "商品"), listFixtureEnd(430, "列表末尾"), "RTX")
	gap := listFixtureViewport(listFixtureRow(150, 200, "商品")+listFixtureRow(420, 500, "被裁切")+listFixtureRow(410, 450, "下一列商品"), "", "RTX")
	hiddenMarker := strings.Replace(valid, `bounds="[0,10][60,90]"`, `bounds="[0,0][0,0]"`, 1)
	for _, tc := range []struct {
		name, viewport, code string
		flags                []string
	}{
		{"footer without matching rows", valid, "APP_LIST_ITEM_UNREADABLE", []string{"--item", `{"class":"wrong.Row"}`}},
		{"clipped row between readable rows", gap, "APP_LIST_ITEM_UNREADABLE", nil},
		{"hidden page marker", hiddenMarker, "APP_LIST_PAGE_CHANGED", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := append([]string{"--end-node", `{"text":"列表末尾"}`}, tc.flags...)
			r, failure, sent := listFixtureRun(t, []string{tc.viewport}, nil, extra...)
			if failure == nil || failure.Code != tc.code || sent != 0 ||
				r.List.ReachedEnd || len(r.List.Items) != 0 {
				t.Fatalf("result=%+v failure=%+v swipes=%d", r.List, failure, sent)
			}
		})
	}
}
