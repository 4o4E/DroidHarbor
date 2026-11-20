package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"droidharbor/internal/adb"
)

const (
	previewTextLimit  = 64 << 10
	previewImageLimit = 2 << 20
	previewHexPage    = 4096
)

type deviceFilePreview struct {
	Kind      string `json:"kind"`
	Size      int64  `json:"size"`
	Text      string `json:"text,omitempty"`
	MIME      string `json:"mime,omitempty"`
	Image     []byte `json:"image,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Version   string `json:"version,omitempty"`
	Bytes     []byte `json:"bytes,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
}

func filePreviewCommand(file string, limit int, offset int64) string {
	directory := path.Dir(file)
	dir, _ := adb.ShellQuote(directory)
	name, _ := adb.ShellQuote("./" + path.Base(file))
	prepare := `[ "$(id -u)" = 0 ] || exit 67` + "\n"
	check := ""
	if adb.IsSharedPath(directory) {
		prepare, check = adb.SharedDirectorySetup, adb.SharedUserDirectoryCheck
	}
	read := "head -c " + strconv.Itoa(limit) + " <&3\n"
	if offset > 0 {
		read = "dd bs=4096 skip=" + strconv.FormatInt(offset/previewHexPage, 10) + " count=1 status=none <&3\n"
	}
	// Recheck the opened descriptor against the physical cwd before any read.
	return prepare + "cd -P " + dir + " 2>/dev/null || exit 66\n" + check + "file=" + name + `
[ ! -L "$file" ] && [ -f "$file" ] && [ -r "$file" ] || exit 66
exec 3< "$file"
fd=/proc/$$/fd/3
cwd=$(pwd -P) || exit 68
if [ ! -f "$fd" ] || [ "$(readlink "$fd")" != "${cwd%/}/${file#./}" ]; then exit 67; fi
size=$(stat -Lc %s "$fd") || exit 68
printf '%s\0' "$size" || exit 68
` + read
}

func decodeFilePreview(out []byte, limit int, offset int64) (deviceFilePreview, bool) {
	header, content, ok := bytes.Cut(out, []byte{0})
	size, err := strconv.ParseInt(string(header), 10, 64)
	if !ok || err != nil || size < 0 || (offset > 0 && offset >= size) || int64(len(content)) != min(size-offset, int64(limit)) {
		return deviceFilePreview{}, false
	}
	if offset > 0 {
		return deviceFilePreview{Kind: "binary", Size: size, Bytes: content, Offset: offset}, true
	}
	binary := deviceFilePreview{Kind: "binary", Size: size, Bytes: content[:min(len(content), previewHexPage)]}
	mediaType := http.DetectContentType(content)
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		if int64(len(content)) == size && size <= previewImageLimit {
			return deviceFilePreview{Kind: "image", Size: size, MIME: mediaType, Image: content}, true
		}
		return binary, true
	}
	if !strings.HasPrefix(mediaType, "text/") && mediaType != "application/xml" && mediaType != "application/octet-stream" {
		return binary, true
	}
	truncated := size > previewTextLimit
	content = content[:min(len(content), previewTextLimit)]
	if truncated {
		// Do not misclassify valid UTF-8 just because the byte cap cut a rune.
		for i := len(content) - 1; i >= max(0, len(content)-utf8.UTFMax); i-- {
			if utf8.RuneStart(content[i]) {
				if !utf8.FullRune(content[i:]) {
					content = content[:i]
				}
				break
			}
		}
	}
	if utf8.Valid(content) && bytes.IndexFunc(content, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' && r != '\f'
	}) == -1 {
		text := deviceFilePreview{Kind: "text", Size: size, Text: string(content), Truncated: truncated}
		if !truncated {
			sum := sha256.Sum256(content)
			text.Version = hex.EncodeToString(sum[:])
		}
		return text, true
	}
	return binary, true
}

func (s *Server) previewSessionFile(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	scope, ok := s.deviceToolsScope(w, r, ctx)
	if !ok {
		return
	}
	defer scope.Close()
	file := r.URL.Query().Get("path")
	if file == "/" || !adb.ValidDevicePath(file) || adb.IsSharedTrashPath(file) {
		s.fail(w, r, 422, "INVALID_DEVICE_FILE", "设备文件路径无效")
		return
	}
	var offset int64
	if raw := r.URL.Query().Get("offset"); raw != "" {
		var err error
		offset, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || offset < 0 || offset%previewHexPage != 0 {
			s.fail(w, r, 422, "INVALID_DEVICE_FILE_OFFSET", "预览偏移必须为 4096 的非负整数倍")
			return
		}
	}
	limit := previewTextLimit
	if offset > 0 {
		limit = previewHexPage
	} else {
		switch strings.ToLower(path.Ext(file)) {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp":
			limit = previewImageLimit
		}
	}
	out, err := s.ADB.RunShell(scope.Context, scope.Link.TransportID, filePreviewCommand(file, limit, offset), int64(limit)+32, scope.Check)
	if err != nil {
		s.fail(w, r, 502, "DEVICE_FILE_PREVIEW_UNAVAILABLE", "未能读取设备文件")
		return
	}
	switch out.ExitCode {
	case 66:
		s.fail(w, r, 404, "DEVICE_FILE_UNAVAILABLE", "文件不存在或不是可读普通文件")
	case 67:
		s.fail(w, r, 403, "DEVICE_FILE_FORBIDDEN", "当前连接无权预览该文件")
	case 0:
		if err := scope.Check(); err != nil {
			s.domainError(w, r, err)
			return
		}
		if data, ok := decodeFilePreview(out.Stdout, limit, offset); ok {
			s.json(w, 200, data)
		} else {
			s.fail(w, r, 502, "DEVICE_FILE_PREVIEW_INVALID", "设备文件未完整返回，请刷新预览")
		}
	default:
		s.fail(w, r, 502, "DEVICE_FILE_PREVIEW_UNAVAILABLE", "未能读取设备文件")
	}
}
