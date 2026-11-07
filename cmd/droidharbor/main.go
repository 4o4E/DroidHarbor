package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/capture"
	"droidharbor/internal/connections"
	"droidharbor/internal/devices"
	"droidharbor/internal/events"
	"droidharbor/internal/identity"
	"droidharbor/internal/leases"
	"droidharbor/internal/media"
	"droidharbor/internal/platform"
	"droidharbor/internal/previews"
	"droidharbor/internal/rtc"
	"droidharbor/internal/service"
	"droidharbor/internal/storage"
	"droidharbor/internal/transfers"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("用法：droidharbor version | migrate | admin-init | serve | adb-serve")
	}
	if args[0] == "adb-serve" {
		return runADBService(args[1:])
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	database := flags.String("database", "data/droidharbor.db", "SQLite 文件")
	adminFile := flags.String("admin-file", "data/admin.json", "管理员摘要配置")
	listen := flags.String("listen", "127.0.0.1:8443", "HTTPS 监听地址")
	origin := flags.String("origin", "", "浏览器使用的 HTTPS origin（必填）")
	cert := flags.String("tls-cert", "", "TLS 证书")
	key := flags.String("tls-key", "", "TLS 私钥")
	web := flags.String("web-dir", "web/dist", "构建后的静态前端目录")
	ttl := flags.Duration("admin-session-ttl", 12*time.Hour, "管理员会话有效期")
	adbAddress := flags.String("adb-server", "", "显式指定的 ADB 管理地址 host:port（serve 必填）")
	adbTimeout := flags.Duration("adb-timeout", 10*time.Second, "ADB 管理请求与身份核验时限")
	scrcpyServer := flags.String("scrcpy-server", "", "锁定版本 scrcpy-server artifact 路径（serve 必填）")
	rtcListen := flags.String("rtc-listen", "", "显式 UDP4 WebRTC 监听 host:port（serve 必填）")
	rtcAdvertisedIP := flags.String("rtc-advertised-ip", "", "浏览器可达的显式 WebRTC IPv4 地址（serve 必填）")
	transferTempDir := flags.String("transfer-temp-dir", "", "受限本地文件传输暂存目录（serve 必填）")
	transferMaxUpload := flags.Int64("transfer-max-upload-bytes", transfers.DefaultMaxUploadBytes, "单文件最大上传字节数")
	transferMaxActive := flags.Int("transfer-max-active", transfers.DefaultMaxActive, "并发文件传输上限")
	transferMaxOutput := flags.Int64("transfer-max-command-output", transfers.DefaultMaxCommandOutput, "ADB 命令输出最大字节数")
	logFile := flags.String("log-file", "", "serve 追加写入的 JSON 日志文件路径")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("存在未识别的位置参数")
	}
	switch args[0] {
	case "version":
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"product": "droidharbor", "version": "0.0.0-dev", "apiMajor": 1, "schemaVersion": storage.SchemaVersion})
	case "admin-init":
		token, err := identity.Initialize(*adminFile)
		if err != nil {
			return err
		}
		// 此命令的专用输出用于一次性交付秘密；不经运行日志。
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"adminToken": token, "credentialFile": *adminFile})
	case "migrate":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		processLock, err := storage.AcquireProcessLock(*database)
		if err != nil {
			return err
		}
		db, err := storage.Open(ctx, *database)
		if err != nil {
			return errors.Join(err, processLock.Close())
		}
		if err = errors.Join(db.Close(), processLock.Close()); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"schemaVersion": storage.SchemaVersion})
	case "serve":
		options := serveOptions{
			database: *database, adminFile: *adminFile, listen: *listen, origin: *origin,
			cert: *cert, key: *key, web: *web, sessionTTL: *ttl, adbAddress: *adbAddress,
			adbTimeout: *adbTimeout, scrcpyServer: *scrcpyServer, rtcListen: *rtcListen,
			rtcAdvertisedIP: *rtcAdvertisedIP, transferTempDir: *transferTempDir, logFile: *logFile,
			transferMaxUpload: *transferMaxUpload, transferMaxActive: *transferMaxActive,
			transferMaxOutput: *transferMaxOutput,
		}
		return service.Run("DroidHarbor", func(ctx context.Context, ready func()) error {
			return runServe(ctx, ready, options)
		})
	default:
		return fmt.Errorf("未知命令 %q", args[0])
	}
}

type serveOptions struct {
	database, adminFile, listen, origin, cert, key, web                string
	sessionTTL                                                         time.Duration
	adbAddress                                                         string
	adbTimeout                                                         time.Duration
	scrcpyServer, rtcListen, rtcAdvertisedIP, transferTempDir, logFile string
	transferMaxUpload                                                  int64
	transferMaxActive                                                  int
	transferMaxOutput                                                  int64
}

func runServe(ctx context.Context, ready func(), options serveOptions) (runErr error) {
	if ctx == nil || ready == nil {
		return errors.New("service context and ready callback are required")
	}
	var err error
	var logOutput = os.Stderr
	var logSink *os.File
	if options.logFile != "" {
		logSink, err = os.OpenFile(options.logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("open service log file: %w", err)
		}
		logOutput = logSink
		defer func() {
			if err := logSink.Close(); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("close service log file: %w", err))
			}
		}()
	}
	logger := slog.New(slog.NewJSONHandler(logOutput, nil))
	parsed, err := url.Parse(options.origin)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("origin 必须是无路径、参数或用户信息的 HTTPS 地址")
	}
	if options.cert == "" || options.key == "" || options.sessionTTL <= 0 {
		return errors.New("必须配置 TLS 证书、私钥和正数会话有效期")
	}
	if options.adbAddress == "" || options.scrcpyServer == "" || options.rtcListen == "" || options.rtcAdvertisedIP == "" || options.transferTempDir == "" {
		return errors.New("serve 必须显式配置 ADB、scrcpy artifact、WebRTC UDP4 地址和文件暂存目录")
	}
	if _, err = tls.LoadX509KeyPair(options.cert, options.key); err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(options.web, "index.html")); err != nil {
		return fmt.Errorf("前端未构建：%w", err)
	}
	credential, err := identity.Load(options.adminFile)
	if err != nil {
		return err
	}
	adbClient, err := adb.NewClient(options.adbAddress, options.adbTimeout)
	if err != nil {
		return err
	}
	processLock, err := storage.AcquireProcessLock(options.database)
	if err != nil {
		return err
	}
	db, err := storage.Open(ctx, options.database)
	if err != nil {
		return errors.Join(err, processLock.Close())
	}
	transferConfig := transfers.Config{
		TempDir:          options.transferTempDir,
		MaxUploadBytes:   options.transferMaxUpload,
		MaxActive:        options.transferMaxActive,
		MaxCommandOutput: options.transferMaxOutput,
	}
	if err = transfers.Recover(ctx, db, transferConfig); err != nil {
		return errors.Join(err, db.Close(), processLock.Close())
	}
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	var registry *devices.Registry
	var tunnels *connections.Manager
	var captureManager *capture.Manager
	var manager *leases.Manager
	var stateEvents *events.Bus
	var previewManager *previews.Manager
	var rtcEndpoint *rtc.Endpoint
	var mediaManager *media.Manager
	var transferManager *transfers.Manager
	closeRuntime := func() error {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		var failures []error
		if transferManager != nil {
			failures = append(failures, transferManager.Close(cleanupCtx))
		}
		if mediaManager != nil {
			failures = append(failures, mediaManager.Close(cleanupCtx))
		}
		if manager != nil {
			failures = append(failures, manager.Close())
		}
		if captureManager != nil {
			failures = append(failures, captureManager.Close(cleanupCtx))
		}
		if rtcEndpoint != nil {
			failures = append(failures, rtcEndpoint.Close())
		}
		if previewManager != nil {
			failures = append(failures, previewManager.Close())
		}
		if tunnels != nil {
			failures = append(failures, tunnels.Close())
		}
		if registry != nil {
			failures = append(failures, registry.Close())
		}
		if stateEvents != nil {
			stateEvents.Close()
		}
		stopRuntime()
		failures = append(failures, db.Close(), processLock.Close())
		return errors.Join(failures...)
	}

	registry = devices.New(runtimeCtx, db, adbClient, logger, options.adbTimeout)
	tunnels = connections.New(runtimeCtx, db, adbClient, registry, logger)
	captureManager, err = capture.New(runtimeCtx, db, adbClient, registry, capture.Config{ServerPath: options.scrcpyServer}, logger)
	if err != nil {
		return errors.Join(err, closeRuntime())
	}
	registry.SetTaskCleanup(func(recoveryCtx context.Context, deviceID string) (bool, error) {
		if err := tunnels.Recover(recoveryCtx, deviceID); err != nil {
			return false, err
		}
		return captureManager.Recover(recoveryCtx, deviceID)
	})
	manager, err = leases.New(runtimeCtx, db, registry, logger)
	if err != nil {
		return errors.Join(err, closeRuntime())
	}
	captureManager.SetBootID(manager.BootID())
	if err := captureManager.SetResourceFailure(manager.ResourceFailure); err != nil {
		return errors.Join(err, closeRuntime())
	}
	transferConfig.BootID = manager.BootID()
	stateEvents = events.New(manager.BootID())
	manager.SetEvents(stateEvents)
	registry.SetEvents(stateEvents)
	rtcEndpoint, err = rtc.New(rtc.Config{ListenAddress: options.rtcListen, AdvertisedIP: options.rtcAdvertisedIP})
	if err != nil {
		return errors.Join(err, closeRuntime())
	}
	mediaManager = media.New(runtimeCtx, media.Dependencies{
		DB: db, ADB: adbClient, Source: registry, Captures: captureManager, Transport: rtcEndpoint,
		Events: stateEvents, TaskOwner: manager.EventTask, AdminVersion: credential.Version,
		Logger: logger,
	})
	transferManager, err = transfers.New(runtimeCtx, transfers.Dependencies{DB: db, ADB: adbClient, Scopes: mediaManager, Logger: logger}, transferConfig)
	if err != nil {
		return errors.Join(err, closeRuntime())
	}
	previewManager = previews.New(runtimeCtx, registry, adbClient)
	registry.Start(manager)
	app := &platform.Server{
		DB: db, Admin: credential, Origin: options.origin, SessionTTL: options.sessionTTL, Logger: logger,
		Leases: manager, Devices: registry, Connections: tunnels, Events: stateEvents, Previews: previewManager,
		Media: mediaManager, Transfers: transferManager,
	}
	server := &http.Server{
		Addr:              options.listen,
		Handler:           app.Handler(http.FileServer(http.Dir(options.web))),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	listener, err := net.Listen("tcp", options.listen)
	if err != nil {
		return errors.Join(err, closeRuntime())
	}
	ready()
	result := make(chan error, 1)
	go func() {
		logger.Info("https_listening", "address", options.listen)
		result <- server.ServeTLS(listener, options.cert, options.key)
	}()
	select {
	case err = <-result:
		stateEvents.Close()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			err = errors.Join(err, server.Close())
		}
	case <-ctx.Done():
		stateEvents.Close()
		shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		err = server.Shutdown(shutdownCtx)
		stop()
		if err != nil {
			err = errors.Join(err, server.Close())
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, closeRuntime())
}
