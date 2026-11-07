package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/service"
)

func runADBService(args []string) error {
	flags := flag.NewFlagSet("adb-serve", flag.ContinueOnError)
	adbPath := flags.String("adb", "", "绝对路径的官方 adb 可执行文件（必填）")
	listen := flags.String("listen", "", "仅回环地址的显式 ADB 监听 host:port（必填）")
	logPath := flags.String("log-file", "", "追加写入的 JSON 服务日志（必填）")
	authSeedDir := flags.String("auth-seed-dir", "", "首次启动时导入的 ADB 授权材料目录（可选，不覆盖已有内容）")
	startupTimeout := flags.Duration("startup-timeout", 10*time.Second, "ADB 服务启动和协议就绪时限")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("存在未识别的 adb-serve 参数")
	}
	if *startupTimeout <= 0 || *startupTimeout > time.Minute {
		return errors.New("startup-timeout 必须大于 0 且不超过 1 分钟")
	}
	if strings.TrimSpace(*logPath) == "" || !filepath.IsAbs(*logPath) {
		return errors.New("log-file must be an absolute path")
	}
	logFile, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open ADB service log file: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(logFile, nil))
	if err := validateADBServiceConfig(*adbPath, *listen, *logPath, *authSeedDir); err != nil {
		logger.Error("adb_service_invalid_configuration", "error", err)
		_ = logFile.Close()
		return err
	}
	logger.Info("adb_service_starting", "listen", *listen)
	err = service.Run("DroidHarborADB", func(ctx context.Context, ready func()) error {
		profile, err := service.CurrentProfileDirectory()
		if err != nil {
			return err
		}
		logger.Info("adb_authorization_profile", "profile", profile, "key", filepath.Join(profile, ".android", "adbkey"))
		if *authSeedDir != "" {
			logger.Info("adb_authorization_seed_configured", "directory", *authSeedDir)
		}
		return runOwnedADB(ctx, *adbPath, *listen, profile, *authSeedDir, *startupTimeout, func() {
			logger.Info("adb_service_ready", "listen", *listen)
			ready()
		})
	})
	if err != nil {
		logger.Error("adb_service_failed", "error", err)
	} else {
		logger.Info("adb_service_stopped")
	}
	if closeErr := logFile.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("close ADB service log file: %w", closeErr))
	}
	return err
}

func validateADBServiceConfig(adbPath, listen, logPath, authSeedDir string) error {
	for _, path := range []struct{ name, value string }{{"adb", adbPath}, {"listen", listen}, {"log-file", logPath}} {
		if strings.TrimSpace(path.value) == "" {
			return fmt.Errorf("%s is required", path.name)
		}
	}
	if !filepath.IsAbs(adbPath) || !filepath.IsAbs(logPath) {
		return errors.New("adb and log-file must be absolute paths")
	}
	if authSeedDir != "" {
		if !filepath.IsAbs(authSeedDir) {
			return errors.New("auth-seed-dir must be an absolute path")
		}
		material, err := readADBAuthDirectory(authSeedDir)
		if err != nil {
			return fmt.Errorf("ADB authorization seed is invalid: %w", err)
		}
		if _, ok := material["adbkey"]; !ok {
			return errors.New("ADB authorization seed has no adbkey private key")
		}
	}
	info, err := os.Stat(adbPath)
	if err != nil || info.IsDir() {
		return fmt.Errorf("adb executable is unavailable")
	}
	host, portText, err := net.SplitHostPort(listen)
	if err != nil {
		return errors.New("listen must be an explicit loopback host:port")
	}
	port, parseErr := strconv.ParseUint(portText, 10, 16)
	if host != "127.0.0.1" || parseErr != nil || port == 0 {
		return errors.New("listen must use 127.0.0.1 and a valid nonzero port")
	}
	return nil
}

func runOwnedADB(ctx context.Context, adbPath, listen, profileDir, authSeedDir string, startupTimeout time.Duration, ready func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	preflight, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("ADB service port is unavailable: %w", err)
	}
	if err = preflight.Close(); err != nil {
		return fmt.Errorf("could not release ADB service preflight listener: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := importADBAuthSeed(profileDir, authSeedDir); err != nil {
		return fmt.Errorf("import ADB authorization seed: %w", err)
	}
	diagnostics := &boundedOutput{limit: 8 << 10}
	// The pinned official SDK accepts tcp:<port>, not tcp:<host>:<port>.
	// Without -a the server stays on loopback on both supported platforms.
	_, port, _ := net.SplitHostPort(listen)
	endpoint := "tcp:" + port
	process, err := service.StartOwnedProcess(adbPath,
		[]string{"-L", endpoint, "nodaemon", "server"},
		dedicatedADBEnvironment(os.Environ()), diagnostics)
	if err != nil {
		return fmt.Errorf("could not start owned ADB server: %w", err)
	}
	stopOnCancellation := context.AfterFunc(ctx, func() { _ = process.Kill() })
	defer stopOnCancellation()
	exited := make(chan error, 1)
	go func() { exited <- process.Wait() }()
	client, err := adb.NewClient(listen, min(startupTimeout, time.Second))
	if err != nil {
		_ = process.Kill()
		<-exited
		return processFailure("could not create ADB readiness client", err, diagnostics)
	}
	deadline := time.NewTimer(startupTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		version, versionErr := client.Version(ctx)
		if versionErr == nil && version > 0 {
			confirm := time.NewTimer(150 * time.Millisecond)
			select {
			case processErr := <-exited:
				confirm.Stop()
				return processFailure("owned ADB server exited before readiness", processErr, diagnostics)
			case <-ctx.Done():
				confirm.Stop()
				<-exited
				return nil
			case <-deadline.C:
				confirm.Stop()
				_ = process.Kill()
				<-exited
				return processFailure("owned ADB server did not become ready before the startup deadline", nil, diagnostics)
			case <-confirm.C:
			}
			version, versionErr = client.Version(ctx)
			if versionErr == nil && version > 0 {
				select {
				case processErr := <-exited:
					return processFailure("owned ADB server exited before readiness", processErr, diagnostics)
				default:
					ready()
					return waitForOwnedADB(ctx, exited, diagnostics)
				}
			}
		}
		select {
		case processErr := <-exited:
			return processFailure("owned ADB server exited before readiness", processErr, diagnostics)
		case <-ctx.Done():
			<-exited
			return nil
		case <-deadline.C:
			_ = process.Kill()
			<-exited
			return processFailure("owned ADB server did not become ready before the startup deadline", nil, diagnostics)
		case <-poll.C:
		}
	}
}

func waitForOwnedADB(ctx context.Context, exited <-chan error, diagnostics *boundedOutput) error {
	select {
	case processErr := <-exited:
		return processFailure("owned ADB server exited unexpectedly", processErr, diagnostics)
	case <-ctx.Done():
		<-exited
		return nil
	}
}

func processFailure(message string, processErr error, diagnostics *boundedOutput) error {
	detail := diagnostics.String()
	if processErr == nil {
		if detail == "" {
			return errors.New(message)
		}
		return fmt.Errorf("%s (ADB stderr: %s)", message, detail)
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", message, processErr)
	}
	return fmt.Errorf("%s: %w (ADB stderr: %s)", message, processErr, detail)
}

type boundedOutput struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	b.mu.Lock()
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		b.data = append(b.data, data[:min(len(data), remaining)]...)
	}
	b.mu.Unlock()
	return len(data), nil
}

func (b *boundedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}

const (
	maxADBAuthFiles = 16
	maxADBAuthFile  = 1 << 20
	maxADBAuthTotal = 4 << 20
)

func readADBAuthDirectory(directory string) (map[string][]byte, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("authorization seed must be a regular directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	if len(entries) > maxADBAuthFiles {
		return nil, errors.New("authorization directory contains too many files")
	}
	material := make(map[string][]byte, len(entries))
	total := 0
	for _, entry := range entries {
		if filepath.Base(entry.Name()) != entry.Name() || strings.ContainsAny(entry.Name(), `/\:`) {
			return nil, fmt.Errorf("authorization item %q has an invalid file name", entry.Name())
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > maxADBAuthFile {
			return nil, fmt.Errorf("authorization item %q must be a bounded regular file", entry.Name())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if len(data) != int(info.Size()) {
			return nil, fmt.Errorf("authorization item %q changed while being read", entry.Name())
		}
		total += len(data)
		if total > maxADBAuthTotal {
			return nil, errors.New("authorization directory exceeds the total size limit")
		}
		material[entry.Name()] = data
	}
	return material, nil
}

func importADBAuthSeed(profileDir, seedDir string) error {
	if seedDir == "" {
		return nil
	}
	seed, err := readADBAuthDirectory(seedDir)
	if err != nil {
		return err
	}
	if _, ok := seed["adbkey"]; !ok {
		return errors.New("authorization seed has no adbkey private key")
	}
	target := filepath.Join(profileDir, ".android")
	current, err := readADBAuthDirectory(target)
	if err == nil {
		if sameADBAuthMaterial(current, seed) {
			return nil
		}
		if len(current) != 0 {
			return errors.New("existing ADB authorization differs from seed; refusing to overwrite")
		}
		return writeADBAuthSeed(target, seed, false)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = os.Mkdir(target, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			current, readErr := readADBAuthDirectory(target)
			if readErr == nil && sameADBAuthMaterial(current, seed) {
				return nil
			}
			return errors.New("ADB authorization directory appeared during seed import; refusing to overwrite")
		}
		return err
	}
	return writeADBAuthSeed(target, seed, true)
}

func sameADBAuthMaterial(left, right map[string][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for name, data := range left {
		if !bytes.Equal(data, right[name]) {
			return false
		}
	}
	return true
}

func writeADBAuthSeed(target string, material map[string][]byte, removeDirectoryOnFailure bool) (result error) {
	names := make([]string, 0, len(material))
	for name := range material {
		names = append(names, name)
	}
	sort.Strings(names)
	var created []string
	defer func() {
		if result == nil {
			return
		}
		for _, name := range created {
			_ = os.Remove(filepath.Join(target, name))
		}
		if removeDirectoryOnFailure {
			_ = os.Remove(target)
		}
	}()
	for _, name := range names {
		path := filepath.Join(target, name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		created = append(created, name)
		n, writeErr := file.Write(material[name])
		if writeErr == nil && n != len(material[name]) {
			writeErr = errors.New("short write while importing ADB authorization")
		}
		if writeErr == nil {
			writeErr = file.Sync()
		}
		if closeErr := file.Close(); writeErr == nil {
			writeErr = closeErr
		}
		if writeErr != nil {
			return writeErr
		}
	}
	return nil
}
func dedicatedADBEnvironment(parent []string) []string {
	env := make([]string, 0, len(parent)+1)
	for _, item := range parent {
		name, _, ok := strings.Cut(item, "=")
		if !ok {
			env = append(env, item)
			continue
		}
		switch strings.ToUpper(name) {
		case "ADB_VENDOR_KEYS", "ADB_SERVER_SOCKET", "ADB_SERVER_PORT", "ANDROID_SDK_HOME", "ANDROID_USER_HOME", "ADB_MDNS_AUTO_CONNECT":
			continue
		}
		env = append(env, item)
	}
	return append(env, "ADB_MDNS_AUTO_CONNECT=")
}
