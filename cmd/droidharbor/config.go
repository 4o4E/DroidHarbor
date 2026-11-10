package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"droidharbor/internal/service"
	"droidharbor/internal/transfers"
	"github.com/joho/godotenv"
)

func runPlatform(args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	file := flags.String("env-file", filepath.Join(filepath.Dir(executable), ".env"), "配置文件路径，默认读取 EXE 同目录的 .env")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("serve 只接受可选的 --env-file，运行参数请填写 .env")
	}
	options, err := loadServeOptions(*file)
	if err != nil {
		return err
	}
	return service.Run("DroidHarbor", func(ctx context.Context, ready func()) error { return runServe(ctx, ready, options) })
}

// Paths are relative to .env, not the caller's current directory. Process
// environment takes precedence, so service/container configuration uses the same keys.
func loadServeOptions(path string) (serveOptions, error) {
	var options serveOptions
	absolute, err := filepath.Abs(path)
	if err != nil {
		return options, err
	}
	data, err := os.ReadFile(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return options, fmt.Errorf("缺少配置文件 %s；请将 .env.example 复制为 .env 并填写必填项", absolute)
	}
	if err != nil {
		return options, fmt.Errorf("读取 .env 失败：%w", err)
	}
	values, err := godotenv.Parse(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
	if err != nil {
		// Parser errors may quote complete lines, including unrelated secrets.
		return options, fmt.Errorf("配置文件 %s 格式无效，请对照 .env.example 检查 KEY=VALUE、引号和换行", absolute)
	}
	get := func(name, fallback string) string {
		if value, ok := os.LookupEnv(name); ok {
			return value
		}
		if value, ok := values[name]; ok {
			return value
		}
		return fallback
	}
	localPath := func(name, fallback string) string {
		value := get(name, fallback)
		if value == "" || filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(filepath.Dir(absolute), value)
	}
	options.database = localPath("DROIDHARBOR_DATABASE", "data/droidharbor.db")
	options.adminFile = localPath("DROIDHARBOR_ADMIN_FILE", "data/admin.json")
	options.listen = get("DROIDHARBOR_LISTEN", "0.0.0.0:8080")
	options.adbAddress = get("DROIDHARBOR_ADB_SERVER", "127.0.0.1:5037")
	options.rtcListen = get("DROIDHARBOR_RTC_LISTEN", "0.0.0.0:8081")
	options.rtcAdvertisedIP = strings.TrimSpace(get("DROIDHARBOR_RTC_ADVERTISED_IP", ""))
	options.transferTempDir = localPath("DROIDHARBOR_TRANSFER_TEMP_DIR", "data/transfers")
	options.logFile = localPath("DROIDHARBOR_LOG_FILE", "logs/platform.jsonl")
	options.logLevel = get("DROIDHARBOR_LOG_LEVEL", "info")
	options.web = localPath("DROIDHARBOR_WEB_DIR", "")
	options.scrcpyServer = localPath("DROIDHARBOR_SCRCPY_SERVER", "")
	if options.rtcAdvertisedIP != "" {
		if ip := net.ParseIP(options.rtcAdvertisedIP); ip == nil || ip.To4() == nil {
			return options, errors.New("DROIDHARBOR_RTC_ADVERTISED_IP 必须是 IPv4 地址")
		}
	}
	for _, value := range []struct {
		name, fallback string
		target         *time.Duration
	}{
		{"DROIDHARBOR_ADMIN_SESSION_TTL", "12h", &options.sessionTTL},
		{"DROIDHARBOR_ADB_TIMEOUT", "10s", &options.adbTimeout},
	} {
		parsed, err := time.ParseDuration(get(value.name, value.fallback))
		if err != nil || parsed <= 0 {
			return options, fmt.Errorf("%s 必须是正数时长，例如 10s 或 12h", value.name)
		}
		*value.target = parsed
	}
	for _, value := range []struct {
		name     string
		fallback int64
		target   *int64
	}{
		{"DROIDHARBOR_TRANSFER_MAX_UPLOAD_BYTES", transfers.DefaultMaxUploadBytes, &options.transferMaxUpload},
		{"DROIDHARBOR_TRANSFER_MAX_COMMAND_OUTPUT", transfers.DefaultMaxCommandOutput, &options.transferMaxOutput},
	} {
		parsed, err := strconv.ParseInt(get(value.name, strconv.FormatInt(value.fallback, 10)), 10, 64)
		if err != nil || parsed <= 0 {
			return options, fmt.Errorf("%s 必须是正整数", value.name)
		}
		*value.target = parsed
	}
	options.transferMaxActive, err = strconv.Atoi(get("DROIDHARBOR_TRANSFER_MAX_ACTIVE", strconv.Itoa(transfers.DefaultMaxActive)))
	if err != nil || options.transferMaxActive <= 0 {
		return options, errors.New("DROIDHARBOR_TRANSFER_MAX_ACTIVE 必须是正整数")
	}
	return options, nil
}
