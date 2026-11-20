package platform

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/identity"
	"droidharbor/internal/media"
)

var trashID = regexp.MustCompile(`^[a-f0-9]{64}$`)

const trashDir = `trash="$shared/` + ".droidharbor-trash" + `"
`
const trashCheck = `
[ -d "$trash" ] && [ ! -L "$trash" ] || exit 67
`

const trashListCommand = adb.SharedDirectorySetup + trashDir + `
if [ ! -e "$trash" ] && [ ! -L "$trash" ]; then exit 0; fi
` + trashCheck + `
cd -P "$trash" || exit 67
for entry in *; do
  [ -d "$entry" ] && [ ! -L "$entry" ] || continue
  [ -f "$entry/origin" ] && [ ! -L "$entry/origin" ] || continue
  if [ -L "$entry/item" ]; then continue
  elif [ -d "$entry/item" ]; then kind=directory
  elif [ -f "$entry/item" ]; then kind=file
  else continue; fi
  origin=$(head -c 1369 "$entry/origin") || exit 68
  when=$(stat -c %Y -- "$entry") || exit 68
  printf '%s\0%s\0%s\0%s\0' "$entry" "$origin" "$kind" "$when" || exit 68
done
`

type trashEntry struct {
	ID           string `json:"id"`
	OriginalPath string `json:"originalPath"`
	Kind         string `json:"kind"`
	DeletedAt    int64  `json:"deletedAt"`
}

func validSharedEntry(file string) bool {
	return strings.HasPrefix(file, adb.SharedStorage+"/") && adb.ValidDevicePath(file) && !adb.IsSharedTrashPath(file)
}

func validDeviceName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && !strings.ContainsAny(name, "/\\\x00")
}

func parseTrashOrigin(encoded string) (string, bool) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	return string(data), err == nil && validSharedEntry(string(data))
}

func decodeTrashEntries(data []byte) ([]trashEntry, bool) {
	entries := []trashEntry{}
	if len(data) == 0 {
		return entries, true
	}
	body, ok := bytes.CutSuffix(data, []byte{0})
	fields := bytes.Split(body, []byte{0})
	if !ok || len(fields)%4 != 0 {
		return nil, false
	}
	for i := 0; i < len(fields); i += 4 {
		id, kind := string(fields[i]), string(fields[i+2])
		origin, valid := parseTrashOrigin(string(fields[i+1]))
		when, err := strconv.ParseInt(string(fields[i+3]), 10, 64)
		if !trashID.MatchString(id) || !valid || (kind != "file" && kind != "directory") || err != nil {
			return nil, false
		}
		entries = append(entries, trashEntry{ID: id, OriginalPath: origin, Kind: kind, DeletedAt: when})
	}
	slices.SortFunc(entries, func(a, b trashEntry) int {
		if a.DeletedAt != b.DeletedAt {
			if a.DeletedAt > b.DeletedAt {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return entries, true
}

func (s *Server) listFileTrash(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	scope, ok := s.deviceToolsScope(w, r, ctx)
	if !ok {
		return
	}
	defer scope.Close()
	out, err := s.ADB.RunShell(scope.Context, scope.Link.TransportID, trashListCommand, deviceToolsOutput, scope.Check)
	if err != nil || out.ExitCode != 0 {
		s.fail(w, r, 502, "DEVICE_TRASH_UNAVAILABLE", "未能读取手机回收站", shellFailure(out, err))
		return
	}
	if err := scope.Check(); err != nil {
		s.domainError(w, r, err)
		return
	}
	entries, ok := decodeTrashEntries(out.Stdout)
	if !ok {
		s.fail(w, r, 502, "DEVICE_TRASH_INVALID", "手机回收站索引不完整", shellFailure(out, nil))
		return
	}
	s.json(w, 200, entries)
}

const requireRegularEntry = `[ ! -L "$item" ] && { [ -f "$item" ] || [ -d "$item" ]; } || exit 66
`
const requireNoTarget = `[ ! -e "$target" ] && [ ! -L "$target" ] || exit 73
`
const moveWithoutOverwrite = requireNoTarget + `
fileDevice=$(stat -c %d -- "$item") || exit 66
targetDevice=$(stat -c %d -- "${target%/*}") || exit 66
[ "$fileDevice" = "$targetDevice" ] || exit 67
mv -nT -- "$item" "$target" || exit 71
[ ! -e "$item" ] && [ ! -L "$item" ] && [ -e "$target" ] && [ ! -L "$target" ] || exit 71
`
const removeEntry = `
if [ -d "$item" ]; then
  rm -r -- "$item" || exit 71
else
  rm -- "$item" || exit 71
fi
[ ! -e "$item" ] && [ ! -L "$item" ] || exit 71
`

func sharedWorkingDirectory(directory string) string {
	q, _ := adb.ShellQuote(directory)
	return "cd -P " + q + " 2>/dev/null || exit 66\n" + adb.SharedUserDirectoryCheck
}

func operationItem(file string) string {
	name, _ := adb.ShellQuote("./" + path.Base(file))
	return sharedWorkingDirectory(path.Dir(file)) + "item=" + name + "\n"
}

func trashItemSetup(id string) string {
	return trashDir + trashCheck + `entry="$trash/` + id + `"
[ -d "$entry" ] && [ ! -L "$entry" ] || exit 66
[ -f "$entry/origin" ] && [ ! -L "$entry/origin" ] || exit 66
item="$entry/item"
` + requireRegularEntry
}

func shellRecycleEntry(file, id string) string {
	origin := base64.StdEncoding.EncodeToString([]byte(file))
	return adb.SharedDirectorySetup + operationItem(file) + requireRegularEntry + trashDir + `
if [ ! -e "$trash" ] && [ ! -L "$trash" ]; then mkdir -- "$trash" || [ -d "$trash" ] || exit 68; fi
` + trashCheck + `
entry="$trash/` + id + `"
mkdir -- "$entry" || exit 68
trap 'if [ ! -e "$entry/item" ] && [ ! -L "$entry/item" ]; then rm -f -- "$entry/origin" && rmdir -- "$entry" || exit 74; fi' EXIT
printf '%s' '` + origin + `' > "$entry/origin" || exit 68
target="$entry/item"
` + moveWithoutOverwrite
}

const cleanupTrashEntry = `rm -- "$entry/origin" && rmdir -- "$entry" || exit 74
`

func (s *Server) readTrashOrigin(w http.ResponseWriter, r *http.Request, scope media.Scope, id string) (string, bool) {
	command := adb.SharedDirectorySetup + trashItemSetup(id) + `head -c 1369 "$entry/origin"`
	out, err := s.ADB.RunShell(scope.Context, scope.Link.TransportID, command, 1400, scope.Check)
	if err == nil && out.ExitCode == 66 {
		s.fail(w, r, 404, "TRASH_ENTRY_MISSING", "回收站条目已不存在")
		return "", false
	}
	if err != nil || out.ExitCode != 0 {
		s.fail(w, r, 502, "DEVICE_TRASH_UNAVAILABLE", "未能核对回收站位置，未派发还原", shellFailure(out, err))
		return "", false
	}
	if _, ok := parseTrashOrigin(string(out.Stdout)); !ok {
		s.fail(w, r, 502, "DEVICE_TRASH_INVALID", "手机回收站索引不完整", shellFailure(out, nil))
		return "", false
	}
	return string(out.Stdout), true
}

func (s *Server) mutateSessionFiles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	scope, ok := s.deviceToolsScope(w, r, ctx)
	if !ok {
		return
	}
	defer scope.Close()
	var req struct {
		Action    string `json:"action"`
		Path      string `json:"path"`
		Name      string `json:"name"`
		ID        string `json:"id"`
		Confirmed bool   `json:"confirmed"`
	}
	if !s.decode(w, r, &req, false) {
		return
	}
	invalid := func() { s.fail(w, r, 422, "INVALID_FILE_ACTION", "路径、文件名或确认条件无效") }
	command := ""
	switch req.Action {
	case "mkdir":
		if !adb.ValidDevicePath(req.Path) || !adb.IsSharedPath(req.Path) || !validDeviceName(req.Name) || !validSharedEntry(path.Join(req.Path, req.Name)) {
			invalid()
			return
		}
		name, _ := adb.ShellQuote("./" + req.Name)
		command = adb.SharedDirectorySetup + sharedWorkingDirectory(req.Path) + "target=" + name + "\n" + requireNoTarget + `mkdir -- "$target" || exit 68`
	case "rename":
		if !validSharedEntry(req.Path) || !validDeviceName(req.Name) || !validSharedEntry(path.Join(path.Dir(req.Path), req.Name)) {
			invalid()
			return
		}
		name, _ := adb.ShellQuote("./" + req.Name)
		command = adb.SharedDirectorySetup + operationItem(req.Path) + requireRegularEntry + "target=" + name + "\n" + moveWithoutOverwrite
	case "trash":
		if !validSharedEntry(req.Path) {
			invalid()
			return
		}
		command = shellRecycleEntry(req.Path, identity.Secret())
	case "delete":
		if !validSharedEntry(req.Path) || !req.Confirmed {
			invalid()
			return
		}
		command = adb.SharedDirectorySetup + operationItem(req.Path) + requireRegularEntry + removeEntry
	case "restore":
		if !trashID.MatchString(req.ID) {
			invalid()
			return
		}
		origin, found := s.readTrashOrigin(w, r, scope, req.ID)
		if !found {
			return
		}
		file, _ := parseTrashOrigin(origin)
		name, _ := adb.ShellQuote("./" + path.Base(file))
		command = adb.SharedDirectorySetup + sharedWorkingDirectory(path.Dir(file)) + trashItemSetup(req.ID) +
			`[ "$(head -c 1369 "$entry/origin")" = '` + origin + `' ] || exit 66` + "\ntarget=" + name + "\n" + moveWithoutOverwrite + cleanupTrashEntry
	case "purge":
		if !trashID.MatchString(req.ID) || !req.Confirmed {
			invalid()
			return
		}
		command = adb.SharedDirectorySetup + trashItemSetup(req.ID) + removeEntry + cleanupTrashEntry
	default:
		invalid()
		return
	}
	out, err := s.ADB.RunShell(scope.Context, scope.Link.TransportID, command, 16<<10, scope.Check)
	switch {
	case err != nil:
		s.fail(w, r, 502, "DEVICE_FILE_RESULT_UNKNOWN", "手机未返回操作终态；请刷新核对，不要直接重试", err)
	case out.ExitCode == 0:
		if err := scope.Check(); err != nil {
			s.fail(w, r, 502, "DEVICE_FILE_RESULT_UNKNOWN", "会话已变化，请核对手机上的操作结果", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case out.ExitCode == 66:
		s.fail(w, r, 404, "DEVICE_FILE_UNAVAILABLE", "目标不存在、已变化或不是常规文件或目录")
	case out.ExitCode == 67:
		s.fail(w, r, 403, "DEVICE_FILE_FORBIDDEN", "仅可操作共享存储，无法跨文件系统移动")
	case out.ExitCode == 73:
		s.fail(w, r, 409, "DEVICE_FILE_EXISTS", "目标位置有同名项目，没有覆盖")
	case out.ExitCode == 74:
		s.fail(w, r, 502, "DEVICE_TRASH_CLEANUP_FAILED", "回收站索引清理不完整，请刷新核对", shellFailure(out, nil))
	case out.ExitCode == 71:
		s.fail(w, r, 502, "DEVICE_FILE_RESULT_UNKNOWN", "未确认操作完整完成；请刷新核对，不要直接重试", shellFailure(out, nil))
	default:
		s.fail(w, r, 502, "DEVICE_FILE_ACTION_FAILED", "手机拒绝了文件操作", shellFailure(out, nil))
	}
}
