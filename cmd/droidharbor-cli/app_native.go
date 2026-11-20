package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"image/png"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	appProbeLimit = 4 * 1024 * 1024
	appPNGLimit   = 32 * 1024 * 1024
	appDumpScript = `umask 077; p=$(mktemp /data/local/tmp/droidharbor-ui.XXXXXXXX) || exit 1; trap 'rm -f "$p" || exit 1' EXIT; uiautomator dump --compressed "$p" >&2 && cat "$p"`
)

var (
	appPackagePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)
	appClassPattern   = regexp.MustCompile(`^\.?[A-Za-z_$][A-Za-z0-9_$.]*$`)
	appFocusedWindow  = regexp.MustCompile(`^mCurrentFocus\s*=\s*Window\{([^ }]+)\s+u(\d+)\s+([^ }]+)`)
	appNodeBounds     = regexp.MustCompile(`^\[([0-9]+),([0-9]+)\]\[([0-9]+),([0-9]+)\]$`)
	appEditorPackage  = regexp.MustCompile(`(?:^|\s)packageName=([^\s]+)`)
	appEditorType     = regexp.MustCompile(`(?:^|\s)inputType=(0x[0-9a-fA-F]+|[0-9a-fA-F]+)(?:\s|$)`)
	appEditorCursor   = regexp.MustCompile(`(?:^|\s)mCursorSel(Start|End)=(-?[0-9]+)`)
)

func appComponent(value string) (pkg, component string) {
	pkg, class, ok := strings.Cut(value, "/")
	if !ok || !appPackagePattern.MatchString(pkg) || !appClassPattern.MatchString(class) {
		return "", ""
	}
	if class[0] == '.' {
		class = pkg + class
	}
	return pkg, pkg + "/" + class
}

// Android parses a shell command after ADB serializes the local arguments.
func appShellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }

type appForeground struct {
	Package   string `json:"package,omitempty"`
	Component string `json:"component,omitempty"`
	Focused   bool   `json:"focused"`
	window    string
	user      string
}

func parseAppForeground(output []byte) (appForeground, error) {
	var observed appForeground
	found := false
	for line := range bytes.Lines(output) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("mCurrentFocus")) {
			continue
		}
		if name, value, ok := bytes.Cut(line, []byte("=")); ok && string(bytes.TrimSpace(name)) == "mCurrentFocus" && string(bytes.TrimSpace(value)) == "null" {
			found = true
			continue
		}
		match := appFocusedWindow.FindSubmatch(line)
		if match == nil {
			continue
		}
		candidate := appForeground{Focused: true, window: string(match[1]), user: string(match[2])}
		label := string(match[3])
		candidate.Package, candidate.Component = appComponent(label)
		if candidate.Package == "" && appPackagePattern.MatchString(label) {
			candidate.Package = label
		}
		if observed.Focused && observed != candidate {
			return appForeground{}, errors.New("当前获得焦点的窗口不唯一")
		}
		observed, found = candidate, true
	}
	if !found {
		return appForeground{}, errors.New("没有读到当前获得焦点的窗口")
	}
	return observed, nil
}

type appNode struct {
	Package     string `json:"-"`
	ID          string `json:"id,omitempty"`
	Class       string `json:"class"`
	Text        string `json:"text"`
	Description string `json:"description"`
	Bounds      string `json:"bounds"`
	Enabled     bool   `json:"enabled"`
	Focused     bool   `json:"focused"`
	Clickable   bool   `json:"clickable"`
	Selected    bool   `json:"selected"`
	Password    bool   `json:"-"`
	Editable    *bool  `json:"editable,omitempty"`
	ShowingHint bool   `json:"-"`
	HintKnown   bool   `json:"-"`
}

func (n appNode) editField() bool {
	if !n.Enabled || !n.Focused || n.Password {
		return false
	}
	if n.Editable != nil {
		return *n.Editable
	}
	// Stock uiautomator XML does not include isEditable. The input method's
	// current editor must also agree before the command dispatches any keys.
	return n.Class == "android.widget.EditText"
}

func (n appNode) center() (x, y int, ok bool) {
	bounds := appNodeBounds.FindStringSubmatch(n.Bounds)
	if bounds == nil {
		return 0, 0, false
	}
	var p [4]int
	for i := range p {
		v, err := strconv.ParseUint(bounds[i+1], 10, 16)
		if err != nil {
			return 0, 0, false
		}
		p[i] = int(v)
	}
	return p[0] + (p[2]-p[0])/2, p[1] + (p[3]-p[1])/2, p[0] < p[2] && p[1] < p[3]
}

func parseAppNodes(data []byte, pkg string) ([]appNode, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	var nodes []appNode
	depth, visible := 0, 0
	root := false
	for {
		token, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch item := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				if root || item.Name.Local != "hierarchy" {
					return nil, errors.New("界面树根元素无效")
				}
				root = true
			} else if item.Name.Local == "node" {
				var node appNode
				for _, attr := range item.Attr {
					value := attr.Value
					switch attr.Name.Local {
					case "package":
						node.Package = value
					case "resource-id":
						node.ID = value
					case "class":
						node.Class = value
					case "text":
						node.Text = value
					case "content-desc":
						node.Description = value
					case "bounds":
						node.Bounds = value
					case "enabled":
						node.Enabled = value == "true"
					case "focused":
						node.Focused = value == "true"
					case "clickable":
						node.Clickable = value == "true"
					case "selected":
						node.Selected = value == "true"
					case "password":
						node.Password = value == "true"
					case "editable":
						if v, err := strconv.ParseBool(value); err == nil {
							node.Editable = &v
						}
					case "showing-hint-text":
						if v, err := strconv.ParseBool(value); err == nil {
							node.ShowingHint, node.HintKnown = v, true
						}
					}
				}
				visible++
				if node.Package == pkg && !node.Password {
					nodes = append(nodes, node)
				}
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if len(bytes.TrimSpace(item)) > 0 {
				return nil, errors.New("界面树含有非 XML 数据")
			}
		}
	}
	if !root || depth != 0 || visible == 0 {
		return nil, errors.New("界面树为空或未完整返回")
	}
	return nodes, nil
}

type appEditState struct {
	identity   string
	start, end int
}

// Use the active client block, not server-side editor history or the separate
// IME process dump. Stock Android emits the actual served connection here.
func appEditor(data []byte, pkg string) appEditState {
	state := appEditState{start: -1, end: -1}
	active, editor, matchesPackage, textType := false, false, false, false
	for raw := range bytes.Lines(data) {
		line := bytes.TrimSpace(raw)
		switch {
		case bytes.HasPrefix(line, []byte("Input method client state for ")):
			if state.identity != "" {
				return state
			}
			active, editor, matchesPackage, textType = false, false, false, false
		case bytes.HasPrefix(line, []byte("mActive=")):
			active = bytes.Equal(line, []byte("mActive=true")) || bytes.HasPrefix(line, []byte("mActive=true "))
		case bytes.Equal(line, []byte("mCurrentEditorInfo:")), bytes.Equal(line, []byte("mCurrentTextBoxAttribute:")):
			editor, matchesPackage, textType = active, false, false
		case bytes.HasPrefix(line, []byte("mServedInputConnection=")), bytes.HasPrefix(line, []byte("mServedInputConnectionWrapper=")):
			_, connection, _ := bytes.Cut(line, []byte("="))
			connection, _, _ = bytes.Cut(bytes.TrimSpace(connection), []byte(" "))
			if editor && matchesPackage && textType && len(connection) != 0 && !bytes.Equal(connection, []byte("null")) {
				state.identity = string(connection)
			}
			editor = false
		case editor && bytes.HasPrefix(line, []byte("inputType=")):
			kind := appEditorType.FindSubmatch(line)
			if kind != nil {
				n, err := strconv.ParseUint(strings.TrimPrefix(string(kind[1]), "0x"), 16, 32)
				textType = err == nil && n&0xf != 0
			}
		case editor && bytes.HasPrefix(line, []byte("packageName=")):
			p := appEditorPackage.FindSubmatch(line)
			matchesPackage = p != nil && string(p[1]) == pkg
		case state.identity != "" && bytes.HasPrefix(line, []byte("mCursorSel")):
			for _, cursor := range appEditorCursor.FindAllSubmatch(line, 2) {
				if offset, err := strconv.Atoi(string(cursor[2])); err == nil {
					if string(cursor[1]) == "Start" {
						state.start = offset
					} else {
						state.end = offset
					}
				}
			}
			if state.start >= 0 && state.end >= 0 {
				return state
			}
		}
	}
	return state
}

type appExecResult struct {
	stdout, stderr []byte
	started        bool
	err            error
}

type appADB func(ctx context.Context, maxBytes int, args ...string) appExecResult

type appLimitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *appLimitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("原生 ADB 输出超过本次证据上限")
	}
	return b.Buffer.Write(p)
}

func taskAppADB(state *connectionState, env *publicConnectionEnvironment) appADB {
	adbEnv := connectionADBEnvironment(state.ADBHome, state.ADBKeyPath)
	return func(ctx context.Context, maxBytes int, args ...string) appExecResult {
		if err := ctx.Err(); err != nil {
			return appExecResult{err: err}
		}
		if live, err := processIdentityMatches(state.ADBServerPID, state.ADBServerBirth); err != nil || !live {
			return appExecResult{err: errors.New("任务 ADB Server 已退出或归属无法核验")}
		}
		fullArgs := make([]string, len(env.ADBArgs)+len(args))
		copy(fullArgs, env.ADBArgs)
		copy(fullArgs[len(env.ADBArgs):], args)
		command := exec.CommandContext(ctx, env.ADBBinary, fullArgs...)
		command.Env = adbEnv
		out, diagnostic := appLimitedBuffer{limit: maxBytes}, appLimitedBuffer{limit: 16 * 1024}
		command.Stdout, command.Stderr = &out, &diagnostic
		if err := command.Start(); err != nil {
			return appExecResult{err: err}
		}
		err := command.Wait()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return appExecResult{stdout: out.Bytes(), stderr: diagnostic.Bytes(), started: true, err: err}
	}
}

func appCompletePNG(data []byte) bool {
	if _, err := png.DecodeConfig(bytes.NewReader(data)); err != nil {
		return false
	}
	data = data[8:]
	hasImageData := false
	for len(data) >= 12 {
		size := int(binary.BigEndian.Uint32(data))
		if size < 0 || size > len(data)-12 {
			return false
		}
		end := 8 + size
		if crc32.ChecksumIEEE(data[4:end]) != binary.BigEndian.Uint32(data[end:]) {
			return false
		}
		switch string(data[4:8]) {
		case "IDAT":
			hasImageData = hasImageData || size > 0
		case "IEND":
			return hasImageData && size == 0 && len(data) == 12
		}
		data = data[end+4:]
	}
	return false
}

func saveAppPNG(directory string, data []byte) (string, error) {
	if !appCompletePNG(data) {
		return "", errors.New("收到的截图不是完整 PNG")
	}
	file, err := os.CreateTemp(directory, "app-*.png")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err = protectPrivatePath(name); err == nil {
		_, err = file.Write(data)
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return "", errors.Join(err, os.Remove(name))
	}
	return name, nil
}

func appStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func appProbeError(operation string, result appExecResult) error {
	if result.err != nil {
		return fmt.Errorf("%s：%w", operation, result.err)
	}
	return nil
}
