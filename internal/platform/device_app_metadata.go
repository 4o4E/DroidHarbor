package platform

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"droidharbor/internal/media"
)

//go:embed appmetadata/classes.dex.b64
var appMetadataAsset string

var appMetadataBase64 = strings.NewReplacer("\r", "", "\n", "").Replace(appMetadataAsset)

// The component only exists in a private, command-owned directory for the
// duration of the metadata read. ART requires a read-only DEX on Android 14+.
func appMetadataCommand(userID int, mode string, packages string) string {
	return readAndroidUser + "[ \"$user\" = " + strconv.Itoa(userID) + ` ] || exit 69
umask 077
work=$(mktemp -d /data/local/tmp/droidharbor-appmeta.XXXXXXXX) || exit 68
trap 'code=$?; rm -rf -- "$work" || exit 74; exit "$code"' EXIT
trap 'exit 68' HUP INT TERM
printf '%s' '` + appMetadataBase64 + `' | base64 -d > "$work/apps.dex" || exit 68
chmod 400 "$work/apps.dex" || exit 68
CLASSPATH="$work/apps.dex" timeout 14s app_process /system/bin top.e404.droidharbor.appmetadata.Main "$user" ` + mode + packages + ` || exit 68
[ "$(am get-current-user)" = "$user" ] || exit 69
`
}

type androidAppPresentation struct {
	PackageName string `json:"packageName"`
	Version     uint64 `json:"versionCode"`
	Name        string `json:"name"`
	Icon        string `json:"icon"`
}

func (s *Server) readAppMetadata(w http.ResponseWriter, r *http.Request, scope media.Scope, user int, apps []deviceApp, render bool) (map[string]androidAppPresentation, bool) {
	ctx, cancel := context.WithTimeout(scope.Context, 17*time.Second)
	defer cancel()
	mode := "versions"
	if render {
		mode = "render"
	}
	result := make(map[string]androidAppPresentation, len(apps))
	for len(apps) > 0 {
		var args strings.Builder
		n := 0
		// Keep each request well below the ADB service-string limit. Names
		// are validated against the package grammar in the live inventory.
		for n < len(apps) && args.Len()+1+len(apps[n].PackageName) <= 16<<10 {
			args.WriteByte(' ')
			args.WriteString(apps[n].PackageName)
			n++
		}
		out, err := s.ADB.RunShell(ctx, scope.Link.TransportID, appMetadataCommand(user, mode, args.String()), 8<<20, scope.Check)
		switch {
		case err == nil && out.ExitCode == 69:
			s.fail(w, r, 409, "ANDROID_USER_CHANGED", "Android 当前用户已切换，请重新读取应用")
			return nil, false
		case err == nil && out.ExitCode == 74:
			s.fail(w, r, 502, "APP_METADATA_CLEANUP_FAILED", "未能清理手机上的临时应用查询组件")
			return nil, false
		case err != nil || out.ExitCode != 0:
			s.fail(w, r, 502, "APP_METADATA_UNAVAILABLE", "未能读取手机上的应用版本、名称或图标")
			return nil, false
		}
		if err := scope.Check(); err != nil {
			s.domainError(w, r, err)
			return nil, false
		}
		var data struct {
			UserID int                      `json:"userId"`
			Apps   []androidAppPresentation `json:"apps"`
		}
		if json.Unmarshal(out.Stdout, &data) != nil || data.UserID != user || data.Apps == nil {
			s.fail(w, r, 502, "APP_METADATA_INVALID", "手机返回的应用信息不完整")
			return nil, false
		}
		for _, app := range data.Apps {
			result[app.PackageName] = app
		}
		apps = apps[n:]
	}
	return result, true
}

func fillCachedAppPresentations(apps []deviceApp, locale string, entries map[string]cachedAppPresentation) []deviceApp {
	var missing []deviceApp
	for i := range apps {
		app := &apps[i]
		value, exists := entries[app.PackageName]
		if !exists || value.Version != app.version || value.Locale != locale || value.Icon == "" {
			missing = append(missing, *app)
			continue
		}
		app.Name, app.Icon = value.Name, value.Icon
	}
	return missing
}

func (s *Server) applyAppMetadata(w http.ResponseWriter, r *http.Request, scope media.Scope, current *deviceApps) bool {
	if len(current.Apps) == 0 {
		return true
	}
	if current.sdk < 29 {
		versions, ok := s.readAppMetadata(w, r, scope, current.UserID, current.Apps, false)
		if !ok {
			return false
		}
		for i := range current.Apps {
			version, found := versions[current.Apps[i].PackageName]
			if !found {
				s.fail(w, r, 409, "APP_INVENTORY_CHANGED", "手机上的应用清单已变化，请重新读取")
				return false
			}
			current.Apps[i].version = version.Version
		}
	}
	cacheFailed := func() bool {
		s.fail(w, r, 503, "APP_CACHE_UNAVAILABLE", "无法读取或更新运行目录中的应用图标缓存")
		return false
	}
	file, err := s.Cache.appFile(scope.DeviceID, current.UserID)
	if err != nil {
		return cacheFailed()
	}
	entries, err := s.Cache.readApps(file)
	if err != nil {
		return cacheFailed()
	}
	missing := fillCachedAppPresentations(current.Apps, current.locale, entries)
	if len(missing) > 0 {
		rendered, ok := s.readAppMetadata(w, r, scope, current.UserID, missing, true)
		if !ok {
			return false
		}
		additions := make(map[string]cachedAppPresentation, len(missing))
		for _, app := range missing {
			value, found := rendered[app.PackageName]
			if !found || value.Version != app.version || value.Icon == "" {
				s.fail(w, r, 409, "APP_INVENTORY_CHANGED", "手机上的应用清单或版本已变化，请重新读取")
				return false
			}
			additions[app.PackageName] = cachedAppPresentation{Version: value.Version, Locale: current.locale, Name: value.Name, Icon: value.Icon}
		}
		if err := scope.Check(); err != nil {
			s.domainError(w, r, err)
			return false
		}
		if err := s.Cache.mergeApps(file, additions); err != nil {
			return cacheFailed()
		}
		for i := range current.Apps {
			app := &current.Apps[i]
			if value, fresh := additions[app.PackageName]; fresh {
				app.Name, app.Icon = value.Name, value.Icon
			}
		}
	}
	if err := scope.Check(); err != nil {
		s.domainError(w, r, err)
		return false
	}
	return true
}
