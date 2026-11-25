package main

import (
	"context"
	"fmt"
	"image"
	"slices"
	"strings"
	"time"
)

const (
	appListDefaultTimeout = 30 * time.Second
	appListTreeTimeout    = 8 * time.Second
	appListSwipeMillis    = 900
	appListDefaultScreens = 3
	appListMaxScreens     = 8
	appListMaxItems       = 128
	appListMaxBytes       = 48 * 1024 // label bytes plus per-label framing allowance
)

type appListOptions struct {
	item, end  appSelector
	maxScreens int
}

type appListLabel struct {
	Text        string `json:"text,omitempty"`
	Description string `json:"description,omitempty"`
}

type appListItem struct {
	Labels []appListLabel `json:"labels"`
}

type appListResult struct {
	Items      []appListItem `json:"items"`
	Screens    int           `json:"screens"`
	Swipes     int           `json:"swipes"`
	StopReason string        `json:"stopReason"`
	ReachedEnd bool          `json:"reachedEnd"`
}

type appListRow struct {
	appListItem
	bounds image.Rectangle
	bytes  int
}

type appListPage struct {
	list       appNode
	bounds     image.Rectangle
	rows       []appListRow
	endVisible bool
	limited    bool
}

func (r *appRun) listPage(ctx context.Context) (*appObservation, appListPage, *commandFailure) {
	obs, err := r.observeWithTreeBudget(ctx, r.options.pkg, true, appListTreeTimeout)
	if obs != nil {
		r.result.Last = obs
	}
	if err != nil {
		return obs, appListPage{}, appObservationFailure(ctx, err)
	}
	if !r.options.expect.satisfied(obs) {
		r.result.List.StopReason = "page_changed"
		return obs, appListPage{}, appFail("APP_LIST_PAGE_CHANGED", "当前前台或页面控件不符合列表读取条件", 5)
	}
	nodes := obs.nodes
	matches := matchAppNodes(nodes, r.options.target)
	obs.Matches = &matches
	if matches.Count != 1 {
		return obs, appListPage{}, appFail("APP_LIST_TARGET_UNVERIFIED", "列表条件必须匹配唯一控件", 5)
	}
	index := slices.IndexFunc(nodes, r.options.target.matches)
	list := nodes[index]
	bounds := list.rectangle()
	if !list.Scrollable || !list.Enabled || bounds.Empty() || (obs.rotation != "0" && obs.rotation != "1" && obs.rotation != "2" && obs.rotation != "3") {
		return obs, appListPage{}, appFail("APP_LIST_NOT_SCROLLABLE", "未确认列表可滚动且具有有效屏幕范围", 5)
	}
	page := appListPage{list: list, bounds: bounds}
	var labelBytes, lastBottom int
	trailingEdge := false
	for i := index + 1; i < list.end; {
		item := nodes[i]
		descendants := nodes[i:item.end]
		i = item.end
		if item.depth != list.depth+1 || !r.options.list.item.matches(item) {
			continue
		}
		b := item.rectangle()
		if b.Empty() || !b.Overlaps(bounds) {
			trailingEdge = len(page.rows) != 0
			continue
		}
		lastBottom = max(lastBottom, b.Max.Y)
		// Android clamps bounds at the viewport edge, so a row on that edge
		// cannot prove its labels are all visible. Read it when it moves inside.
		if !b.In(bounds) || b.Min.Y == bounds.Min.Y || b.Max.Y == bounds.Max.Y {
			trailingEdge = len(page.rows) != 0
			continue
		}
		if trailingEdge {
			return obs, appListPage{}, appFail("APP_LIST_ITEM_UNREADABLE", "已核验条目之间存在未完整可见的行", 5)
		}
		row := appListRow{bounds: b}
		for _, n := range descendants {
			if strings.TrimSpace(n.Text) == "" && strings.TrimSpace(n.Description) == "" {
				continue
			}
			b := n.rectangle()
			if b.Empty() || !b.In(row.bounds) {
				continue
			}
			row.bytes += 32 + len(n.Text) + len(n.Description)
			if labelBytes+row.bytes > appListMaxBytes {
				page.limited = true
				return obs, page, nil
			}
			row.Labels = append(row.Labels, appListLabel{Text: n.Text, Description: n.Description})
		}
		if len(row.Labels) == 0 {
			return obs, appListPage{}, appFail("APP_LIST_ITEM_UNREADABLE", "可见列表条目没有可核验的文字或描述", 5)
		}
		if len(page.rows) == appListMaxItems {
			page.limited = true
			return obs, page, nil
		}
		labelBytes += row.bytes
		page.rows = append(page.rows, row)
	}
	if len(page.rows) == 0 {
		return obs, appListPage{}, appFail("APP_LIST_ITEM_UNREADABLE", "列表没有完整可见且符合条目条件的内容", 5)
	}
	if end := r.options.list.end; end != (appSelector{}) {
		var count int
		for _, node := range nodes[index+1 : list.end] {
			if !end.matches(node) {
				continue
			}
			b := node.rectangle()
			if !b.Empty() && b.In(bounds) {
				count++
				page.endVisible = b.Min.Y >= lastBottom
			}
		}
		page.endVisible = count == 1 && page.endVisible
	}
	return obs, page, nil
}

func appListSameRow(a, b appListRow) bool { return slices.Equal(a.Labels, b.Labels) }

// A suffix must become the next prefix with observed upward movement. A
// second plausible join is ambiguous, not permission to pick the longer one.
func appListOverlap(before, after []appListRow) (int, string) {
	if slices.EqualFunc(before, after, appListSameRow) {
		return 0, "no_progress"
	}
	overlap := 0
	for size := 1; size <= min(len(before), len(after)); size++ {
		old := before[len(before)-size:]
		if !slices.EqualFunc(old, after[:size], appListSameRow) {
			continue
		}
		moved, valid := false, true
		for i := range old {
			delta := after[i].bounds.Min.Y - old[i].bounds.Min.Y
			if delta > 0 {
				valid = false
				break
			}
			moved = moved || delta < 0
		}
		if valid && moved {
			if overlap != 0 {
				return 0, "overlap_ambiguous"
			}
			overlap = size
		}
	}
	if overlap == 0 {
		return 0, "overlap_missing"
	}
	return overlap, ""
}

func (r *appRun) readList(ctx context.Context) *commandFailure {
	result := &appListResult{Items: []appListItem{}, StopReason: "evidence_unavailable"}
	r.result.List = result
	obs, page, failure := r.listPage(ctx)
	if failure != nil {
		return failure
	}
	bytesRead := 0
	appendRows := func(rows []appListRow) bool {
		for _, row := range rows {
			if len(result.Items) == appListMaxItems || bytesRead+row.bytes > appListMaxBytes {
				return false
			}
			result.Items = append(result.Items, row.appListItem)
			bytesRead += row.bytes
		}
		return true
	}
	appendRows(page.rows)
	result.Screens = 1
	deadline, _ := ctx.Deadline()
	for {
		var stop string
		switch {
		case page.limited:
			stop = "output_limit"
		case page.endVisible:
			stop, result.ReachedEnd = "end_reached", true
		case result.Screens >= r.options.list.maxScreens:
			stop = "screen_limit"
		case time.Until(deadline) <= appListTreeTimeout+(appListSwipeMillis+1000)*time.Millisecond:
			stop = "deadline"
		}
		if stop != "" {
			result.StopReason = stop
			return nil
		}
		front, err := r.foreground(ctx)
		if err != nil {
			result.StopReason = "evidence_unavailable"
			return appObservationFailure(ctx, err)
		}
		if front != obs.Foreground {
			result.StopReason = "page_changed"
			return appFail("APP_LIST_PAGE_CHANGED", "滑动前目标窗口已改变，未发送本次滑动", 5)
		}
		b := page.bounds
		x, y := b.Min.X+b.Dx()/2, b.Min.Y+b.Dy()*7/10
		to := y - b.Dy()*3/10
		if to >= y {
			result.StopReason = "evidence_unavailable"
			return appFail("APP_LIST_NOT_SCROLLABLE", "列表可见区域不足以执行滑动", 5)
		}
		if failure = r.dispatch(ctx, fmt.Sprintf("input swipe %d %d %d %d %d", x, y, x, to, appListSwipeMillis)); failure != nil {
			result.StopReason = "swipe_failed"
			return failure
		}
		result.Swipes++
		nextObs, next, failure := r.listPage(ctx)
		if failure != nil {
			return failure
		}
		if nextObs.Foreground != obs.Foreground || nextObs.rotation != obs.rotation ||
			next.list.ID != page.list.ID || next.list.Class != page.list.Class || next.list.depth != page.list.depth {
			result.StopReason = "page_changed"
			return appFail("APP_LIST_PAGE_CHANGED", "滑动后窗口、屏幕方向或列表身份已改变", 5)
		}
		if next.limited && len(next.rows) == 0 {
			result.StopReason = "output_limit"
			return nil
		}
		overlap, reason := appListOverlap(page.rows, next.rows)
		if reason != "" {
			result.StopReason = reason
			// An unchanged final viewport may reveal a footer after loading;
			// all the rows are already in the result, so no join is needed.
			if reason == "no_progress" && next.endVisible && slices.EqualFunc(page.rows, next.rows, func(a, b appListRow) bool {
				return a.bounds == b.bounds
			}) {
				result.StopReason, result.ReachedEnd = "end_reached", true
			}
			return nil
		}
		result.Screens++
		if !appendRows(next.rows[overlap:]) {
			result.StopReason = "output_limit"
			return nil
		}
		obs, page = nextObs, next
	}
}
