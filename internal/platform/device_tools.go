package platform

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"droidharbor/internal/adb"
	"droidharbor/internal/diagnostics"
	"droidharbor/internal/media"
)

const deviceToolsOutput = 512 << 10

// Shell results are diagnostic only by exit code and byte counts: output can
// contain file contents, package data or paths.
func shellFailure(out adb.ShellResult, err error) error {
	if err != nil {
		return err
	}
	return diagnostics.New(fmt.Sprintf("shell_v2 exit %d, stdout %d bytes, stderr %d bytes", out.ExitCode, len(out.Stdout), len(out.Stderr)))
}

func (s *Server) deviceToolsScope(w http.ResponseWriter, r *http.Request, ctx context.Context) (media.Scope, bool) {
	owner, ok := s.mediaAdmin(w, r, r.Method == http.MethodPost)
	if !ok {
		return media.Scope{}, false
	}
	if s.ADB == nil {
		s.fail(w, r, 503, "DEVICE_TOOLS_UNAVAILABLE", "设备管理暂不可用")
		return media.Scope{}, false
	}
	// Files and installed apps can reveal private information even when read.
	scope, err := s.Media.SessionScope(ctx, owner, r.PathValue("id"), true)
	if err != nil {
		s.domainError(w, r, err)
		return media.Scope{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return scope, true
}

type deviceFile struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size"`
	ModifiedAt int64  `json:"modifiedAt"`
}

type deviceDirectory struct {
	Path          string       `json:"path"`
	RootAvailable bool         `json:"rootAvailable"`
	Writable      bool         `json:"writable"`
	Entries       []deviceFile `json:"entries"`
}

func deviceListCommand(directory string) string {
	quoted, _ := adb.ShellQuote(directory) // ValidDevicePath rejects NUL.
	shared := adb.IsSharedPath(directory)
	prepare := "[ \"$uid\" = 0 ] || exit 67\n"
	check := "writable=0\n"
	if shared {
		prepare = adb.SharedDirectorySetup
		check = adb.SharedUserDirectoryCheck + "writable=0; if [ -w . ]; then writable=1; fi\n"
	}
	return "uid=$(id -u) || exit 68\n" + prepare + "cd -P " + quoted + " 2>/dev/null || exit 66\n" + check + `
[ -r . ] && [ -x . ] || exit 67
printf '%s\0%s\0' "$uid" "$writable" || exit 68
stat_batch() {
  stats=$(stat -c '%f %s %Y' -- "$@") || exit 68
  printf '%s\0' "$#" "$@" "$stats" || exit 68
}
set --
cwd=$(pwd -P) || exit 68
for child in .[!.]* ..?* *; do
  if [ "$cwd" = "$shared" ] && [ "$child" = '.droidharbor-trash' ]; then continue; fi
  [ -e "$child" ] || [ -L "$child" ] || continue
  set -- "$@" "./$child"
  if [ "$#" -eq 64 ]; then
    stat_batch "$@"
    set --
  fi
done
if [ "$#" -gt 0 ]; then stat_batch "$@"; fi
`
}

func decodeDeviceDirectory(directory string, output []byte) (deviceDirectory, bool) {
	data, terminated := bytes.CutSuffix(output, []byte{0})
	if !terminated || !utf8.Valid(data) {
		return deviceDirectory{}, false
	}
	parts := bytes.Split(data, []byte{0})
	if len(parts) < 2 {
		return deviceDirectory{}, false
	}
	uid, err := strconv.ParseUint(string(parts[0]), 10, 32)
	if err != nil || (string(parts[1]) != "0" && string(parts[1]) != "1") {
		return deviceDirectory{}, false
	}
	entries := make([]deviceFile, 0, max(0, len(parts)-2))
	for i := 2; i < len(parts); {
		count, err := strconv.Atoi(string(parts[i]))
		if err != nil || count < 1 || count > 64 || i+count+1 >= len(parts) {
			return deviceDirectory{}, false
		}
		names := parts[i+1 : i+1+count]
		stats := bytes.Split(parts[i+1+count], []byte{'\n'})
		if len(stats) != count {
			return deviceDirectory{}, false
		}
		for j, row := range stats {
			entry, valid := decodeDeviceFile(names[j], row)
			if !valid {
				return deviceDirectory{}, false
			}
			entries = append(entries, entry)
		}
		i += count + 2
	}
	slices.SortFunc(entries, func(a, b deviceFile) int {
		if a.Kind == "directory" && b.Kind != "directory" {
			return -1
		}
		if b.Kind == "directory" && a.Kind != "directory" {
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return deviceDirectory{Path: directory, RootAvailable: uid == 0, Writable: string(parts[1]) == "1", Entries: entries}, true
}

func decodeDeviceFile(rawName, row []byte) (deviceFile, bool) {
	name, ok := strings.CutPrefix(string(rawName), "./")
	fields := bytes.Fields(row)
	if !ok || name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') || len(fields) != 3 {
		return deviceFile{}, false
	}
	mode, modeErr := strconv.ParseUint(string(fields[0]), 16, 32)
	size, sizeErr := strconv.ParseInt(string(fields[1]), 10, 64)
	modified, modifiedErr := strconv.ParseInt(string(fields[2]), 10, 64)
	if modeErr != nil || sizeErr != nil || size < 0 || modifiedErr != nil {
		return deviceFile{}, false
	}
	kind := "other"
	switch mode & 0xf000 {
	case 0x4000:
		kind = "directory"
	case 0x8000:
		kind = "file"
	case 0xa000:
		kind = "link"
	}
	return deviceFile{Name: name, Kind: kind, Size: size, ModifiedAt: modified}, true
}

func (s *Server) listSessionFiles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	scope, ok := s.deviceToolsScope(w, r, ctx)
	if !ok {
		return
	}
	defer scope.Close()
	directory := r.URL.Query().Get("path")
	if directory == "" {
		directory = adb.SharedStorage
	}
	if !adb.ValidDevicePath(directory) {
		s.fail(w, r, 422, "INVALID_DEVICE_DIRECTORY", "设备目录路径无效")
		return
	}
	out, err := s.ADB.RunShell(scope.Context, scope.Link.TransportID, deviceListCommand(directory), deviceToolsOutput, scope.Check)
	if err != nil {
		s.fail(w, r, 502, "DEVICE_DIRECTORY_UNAVAILABLE", "未能读取设备目录", err)
		return
	}
	switch out.ExitCode {
	case 66:
		s.fail(w, r, 404, "DEVICE_DIRECTORY_UNAVAILABLE", "设备目录不存在或无法打开")
	case 67:
		s.fail(w, r, 403, "DEVICE_DIRECTORY_FORBIDDEN", "当前连接无权浏览该目录")
	case 0:
		if err := scope.Check(); err != nil {
			s.domainError(w, r, err)
			return
		}
		value, valid := decodeDeviceDirectory(directory, out.Stdout)
		if !valid {
			s.fail(w, r, 502, "DEVICE_DIRECTORY_INVALID", "设备未返回完整目录", shellFailure(out, nil))
			return
		}
		s.json(w, 200, value)
	default:
		s.fail(w, r, 502, "DEVICE_DIRECTORY_UNAVAILABLE", "未能读取设备目录", shellFailure(out, nil))
	}
}

var devicePackage = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)*$`)

const readAndroidUser = `user=$(am get-current-user) || exit 68
case "$user" in ''|*[!0-9]*) exit 68;; esac
`

const packageListTail = `printf '\n'
pm list packages -3 --user "$user" || exit 68
[ "$(am get-current-user)" = "$user" ] || exit 69
`

const readPackageList = readAndroidUser + `printf '%s\n' "$user"
pm list packages --user "$user" || exit 68
` + packageListTail

// Android 10 exposes the full version code via pm. Older releases use the
// embedded metadata reader for versions without changing the live package list.
const readPackageVersions = readAndroidUser + `printf '%s\n' "$user"
sdk=$(getprop ro.build.version.sdk) || exit 68
case "$sdk" in ''|*[!0-9]*) exit 68;; esac
printf '%s\n' "$sdk"
getprop persist.sys.locale || exit 68
settings --user "$user" get system system_locales || exit 68
if [ "$sdk" -ge 29 ]; then
  pm list packages --show-versioncode --user "$user" || exit 68
else
  pm list packages --user "$user" || exit 68
fi
` + packageListTail

type deviceApp struct {
	PackageName string `json:"packageName"`
	ThirdParty  bool   `json:"thirdParty"`
	Name        string `json:"name,omitempty"`
	Icon        string `json:"icon,omitempty"`
	version     uint64
}

type deviceApps struct {
	UserID int         `json:"userId"`
	Apps   []deviceApp `json:"apps"`
	locale string
	sdk    int
}

func decodeDeviceApps(out []byte, versions bool) (deviceApps, bool) {
	header, lists, ok := strings.Cut(string(out), "\n")
	user, err := strconv.ParseUint(header, 10, 31)
	if !ok || err != nil {
		return deviceApps{}, false
	}
	current := deviceApps{UserID: int(user)}
	if versions {
		sdkText, remaining, ok := strings.Cut(lists, "\n")
		sdk, err := strconv.Atoi(sdkText)
		system, remaining, hasSystem := strings.Cut(remaining, "\n")
		language, rest, hasUser := strings.Cut(remaining, "\n")
		if !ok || err != nil || sdk <= 0 || !hasSystem || !hasUser || len(system)+len(language) > 1024 {
			return deviceApps{}, false
		}
		current.sdk, current.locale = sdk, system+"\n"+language
		lists = rest
	}
	allLines, thirdPartyLines, ok := strings.Cut(lists, "\n\n")
	if !ok && !strings.HasPrefix(lists, "\n") {
		return deviceApps{}, false
	}
	if !ok {
		allLines, thirdPartyLines = "", lists[1:]
	}
	all := make(map[string]deviceApp)
	for line := range strings.SplitSeq(strings.TrimSuffix(allLines, "\n"), "\n") {
		if line == "" && allLines == "" {
			break
		}
		name, ok := strings.CutPrefix(line, "package:")
		if !ok {
			return deviceApps{}, false
		}
		var version uint64
		if current.sdk >= 29 {
			var code string
			name, code, ok = strings.Cut(name, " versionCode:")
			version, err = strconv.ParseUint(code, 10, 64)
			if !ok || err != nil {
				return deviceApps{}, false
			}
		}
		if len(name) > 255 || !devicePackage.MatchString(name) {
			return deviceApps{}, false
		}
		all[name] = deviceApp{PackageName: name, version: version}
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(thirdPartyLines, "\n"), "\n") {
		if line == "" && thirdPartyLines == "" {
			break
		}
		name, ok := strings.CutPrefix(line, "package:")
		app, known := all[name]
		if !ok || !known {
			return deviceApps{}, false
		}
		app.ThirdParty = true
		all[name] = app
	}
	current.Apps = make([]deviceApp, 0, len(all))
	for _, app := range all {
		current.Apps = append(current.Apps, app)
	}
	slices.SortFunc(current.Apps, func(a, b deviceApp) int { return strings.Compare(a.PackageName, b.PackageName) })
	return current, true
}

func (s *Server) sessionApps(w http.ResponseWriter, r *http.Request, scope media.Scope, versions bool) (deviceApps, bool) {
	ctx, cancel := context.WithTimeout(scope.Context, 10*time.Second)
	defer cancel()
	command := readPackageList
	if versions {
		command = readPackageVersions
	}
	out, err := s.ADB.RunShell(ctx, scope.Link.TransportID, command, deviceToolsOutput, scope.Check)
	if err == nil && out.ExitCode == 69 {
		s.fail(w, r, 409, "ANDROID_USER_CHANGED", "Android 当前用户已切换，请重新读取列表")
		return deviceApps{}, false
	}
	if err != nil || out.ExitCode != 0 {
		s.fail(w, r, 502, "DEVICE_APPS_UNAVAILABLE", "无法读取当前 Android 用户的应用", shellFailure(out, err))
		return deviceApps{}, false
	}
	if err := scope.Check(); err != nil {
		s.domainError(w, r, err)
		return deviceApps{}, false
	}
	value, ok := decodeDeviceApps(out.Stdout, versions)
	if !ok {
		s.fail(w, r, 502, "DEVICE_APPS_INVALID", "设备未返回完整应用列表", shellFailure(out, nil))
	}
	return value, ok
}

func (s *Server) listSessionApps(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	scope, ok := s.deviceToolsScope(w, r, ctx)
	if !ok {
		return
	}
	defer scope.Close()
	appScope := r.URL.Query().Get("scope")
	switch appScope {
	case "":
		appScope = "all"
	case "all", "user":
	default:
		s.fail(w, r, 422, "INVALID_APP_SCOPE", "应用范围无效")
		return
	}
	if apps, ok := s.sessionApps(w, r, scope, true); ok {
		if appScope == "user" {
			apps.Apps = slices.DeleteFunc(apps.Apps, func(app deviceApp) bool { return !app.ThirdParty })
		}
		if s.applyAppMetadata(w, r, scope, &apps) {
			s.json(w, 200, apps)
		}
	}
}

type appUninstallResult struct {
	PackageName string `json:"packageName"`
	State       string `json:"state"`
	Message     string `json:"message,omitempty"`
}

func appUninstallCommand(userID int, pkg string) string {
	// pkg is a checked Android identifier and userID is an integer. Verify
	// both before mutation and check the exact user's state, not pm's exit alone.
	return readAndroidUser + "[ \"$user\" = " + strconv.Itoa(userID) + " ] || exit 69\npkg=" + pkg + `
packages=$(pm list packages -3 --user "$user" "$pkg") || exit 68
printf '%s\n' "$packages" | grep -Fqx "package:$pkg" || exit 70
[ "$(am get-current-user)" = "$user" ] || exit 69
if pm uninstall --user "$user" "$pkg" >/dev/null 2>&1; then result=0; else result=1; fi
packages=$(pm list packages --user "$user" "$pkg") || exit 71
if printf '%s\n' "$packages" | grep -Fqx "package:$pkg"; then
  [ "$result" = 1 ] && exit 72
  exit 71
else
  [ "$?" -eq 1 ] || exit 71
fi
case "$packages" in ''|package:*) exit 0;; *) exit 71;; esac
`
}

func (s *Server) uninstallSessionApps(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	scope, ok := s.deviceToolsScope(w, r, ctx)
	if !ok {
		return
	}
	defer scope.Close()
	var body struct {
		UserID   *int     `json:"userId"`
		Packages []string `json:"packages"`
	}
	if !s.decode(w, r, &body, false) {
		return
	}
	if body.UserID == nil || *body.UserID < 0 || len(body.Packages) == 0 || len(body.Packages) > 32 {
		s.fail(w, r, 422, "INVALID_UNINSTALL_BATCH", "请选择 1 至 32 个应用并确认 Android 用户")
		return
	}
	selected := make(map[string]bool, len(body.Packages))
	for _, pkg := range body.Packages {
		if len(pkg) > 255 || !devicePackage.MatchString(pkg) || selected[pkg] {
			s.fail(w, r, 422, "INVALID_UNINSTALL_PACKAGE", "包名无效或重复")
			return
		}
		selected[pkg] = true
	}
	apps, ok := s.sessionApps(w, r, scope, false)
	if !ok {
		return
	}
	if apps.UserID != *body.UserID {
		s.fail(w, r, 409, "ANDROID_USER_CHANGED", "Android 当前用户已切换，请重新选择应用")
		return
	}
	eligible := make(map[string]bool, len(selected))
	for _, app := range apps.Apps {
		if selected[app.PackageName] && app.ThirdParty {
			eligible[app.PackageName] = true
		}
	}
	if len(eligible) != len(selected) {
		s.fail(w, r, 409, "PACKAGE_NOT_UNINSTALLABLE", "选中应用包含系统包或已不在当前用户的应用列表")
		return
	}
	results := make([]appUninstallResult, 0, len(body.Packages))
	stopped := false
	for _, pkg := range body.Packages {
		item := appUninstallResult{PackageName: pkg}
		if stopped || scope.Check() != nil {
			item.State, item.Message = "not_started", "批次已停止，未派发卸载"
		} else {
			cmdCtx, stop := context.WithTimeout(scope.Context, 18*time.Second)
			out, err := s.ADB.RunShell(cmdCtx, scope.Link.TransportID, appUninstallCommand(apps.UserID, pkg), 16<<10, scope.Check)
			stop()
			switch {
			case err != nil:
				item.State, item.Message, stopped = "unknown", "未能核对卸载结果，未重试", true
			case out.ExitCode == 0:
				item.State = "succeeded"
			case out.ExitCode == 69:
				item.State, item.Message, stopped = "not_started", "Android 当前用户已切换", true
			case out.ExitCode == 68 || out.ExitCode == 70:
				item.State, item.Message = "not_started", "未核实当前用户的第三方应用资格"
			case out.ExitCode == 72:
				item.State, item.Message = "failed", "Android 拒绝卸载，应用仍在当前用户中"
			default:
				item.State, item.Message, stopped = "unknown", "未能核对卸载结果，未重试", true
			}
			if item.State != "succeeded" {
				s.Logger.Warn("app_uninstall_item_incomplete", "requestId", r.Context().Value(requestKey{}),
					"deviceId", scope.DeviceID, "sessionId", r.PathValue("id"), "itemNumber", len(results)+1,
					"state", item.State, "reason", item.Message, "cause", diagnostics.Cause(shellFailure(out, err)))
			}
		}
		results = append(results, item)
	}
	s.json(w, 200, struct {
		Results []appUninstallResult `json:"results"`
	}{results})
}
