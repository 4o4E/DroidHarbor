package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/bundle"
	"droidharbor/internal/capture"
	"droidharbor/internal/connections"
	"droidharbor/internal/devices"
	"droidharbor/internal/diagnostics"
	"droidharbor/internal/events"
	"droidharbor/internal/identity"
	"droidharbor/internal/leases"
	"droidharbor/internal/media"
	"droidharbor/internal/platform"
	"droidharbor/internal/previews"
	"droidharbor/internal/rtc"
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
		return runPlatform(nil)
	}
	if args[0] == "adb-serve" {
		return runADBService(args[1:])
	}
	if args[0] == "serve" {
		return runPlatform(args[1:])
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	database := flags.String("database", "data/droidharbor.db", "SQLite 文件")
	adminFile := flags.String("admin-file", "data/admin.json", "管理员摘要配置")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("存在未识别的位置参数")
	}
	switch args[0] {
	case "version":
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"product": "droidharbor", "version": "0.0.0-dev", "apiMajor": 1, "schemaVersion": storage.SchemaVersion, "build": buildInfo()})
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
	default:
		return fmt.Errorf("未知命令 %q", args[0])
	}
}

type serveOptions struct {
	database, adminFile, listen, web                                   string
	sessionTTL                                                         time.Duration
	adbAddress                                                         string
	adbTimeout                                                         time.Duration
	scrcpyServer, rtcListen, rtcAdvertisedIP, transferTempDir, logFile string
	logLevel                                                           string
	transferMaxUpload                                                  int64
	transferMaxActive                                                  int
	transferMaxOutput                                                  int64
}

func runServe(ctx context.Context, ready func(), options serveOptions) (runErr error) {
	if ctx == nil || ready == nil {
		return errors.New("service context and ready callback are required")
	}
	var level slog.Level
	if options.logLevel != "" {
		if err := level.UnmarshalText([]byte(options.logLevel)); err != nil {
			return fmt.Errorf("invalid log-level: %w", err)
		}
	}
	logger, logSink, err := diagnostics.Open(options.logFile, level)
	if err != nil {
		return err
	}
	stage := "configuration"
	defer func() {
		if runErr != nil {
			logger.Error("platform_failed", "stage", stage, "cause", diagnostics.Cause(runErr))
			if stage == "configuration" {
				logger.Error("invalid_configuration", "error", runErr.Error())
			}
		} else {
			logger.Info("platform_stopped")
		}
		runErr = errors.Join(runErr, logSink.Close())
	}()
	logger.Info("platform_starting", "pid", os.Getpid(), "build", buildInfo(), "listen", options.listen, "adbServer", options.adbAddress, "rtcListen", options.rtcListen)
	if options.sessionTTL <= 0 {
		return errors.New("DROIDHARBOR_ADMIN_SESSION_TTL 必须是正数时长")
	}
	if options.adbAddress == "" || options.rtcListen == "" || options.rtcAdvertisedIP == "" || options.transferTempDir == "" {
		return errors.New("请在 .env 配置 ADB、WebRTC IPv4 地址和文件暂存目录")
	}
	if err = os.MkdirAll(options.transferTempDir, 0700); err != nil {
		return fmt.Errorf("创建文件暂存目录失败：%w", err)
	}
	stage = "load_frontend"
	var static http.Handler
	if options.web != "" {
		if _, err = os.Stat(filepath.Join(options.web, "index.html")); err != nil {
			return fmt.Errorf("前端覆盖目录不可用：%w", err)
		}
		static = http.FileServer(http.Dir(options.web))
	} else {
		assets, err := bundle.Frontend()
		if err != nil {
			return fmt.Errorf("open embedded frontend: %w", err)
		}
		static = http.FileServer(http.FS(assets))
	}
	stage = "load_admin_credentials"
	credential, err := identity.Load(options.adminFile)
	if errors.Is(err, os.ErrNotExist) {
		token, initErr := identity.Initialize(options.adminFile)
		if initErr != nil {
			return fmt.Errorf("首次创建管理员凭证失败：%w", initErr)
		}
		// First-start bootstrap uses stdout only; secrets never reach the runtime log.
		if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"event": "administrator_initialized", "adminToken": token, "credentialFile": options.adminFile}); err != nil {
			return fmt.Errorf("输出首次管理员令牌失败：%w", err)
		}
		logger.Info("admin_credentials_initialized")
		credential, err = identity.Load(options.adminFile)
	}
	if err != nil {
		return err
	}
	stage = "configure_adb_client"
	adbClient, err := adb.NewClient(options.adbAddress, options.adbTimeout)
	if err != nil {
		return err
	}
	stage = "acquire_database_lock"
	processLock, err := storage.AcquireProcessLock(options.database)
	if err != nil {
		return err
	}
	stage = "open_database"
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
	stage = "recover_transfers"
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
	stage = "initialize_capture"
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
	stage = "initialize_leases"
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
	stage = "bind_webrtc"
	rtcEndpoint, err = rtc.New(rtc.Config{ListenAddress: options.rtcListen, AdvertisedIP: options.rtcAdvertisedIP})
	if err != nil {
		return errors.Join(err, closeRuntime())
	}
	mediaManager = media.New(runtimeCtx, media.Dependencies{
		DB: db, ADB: adbClient, Source: registry, Captures: captureManager, Transport: rtcEndpoint,
		Events: stateEvents, TaskOwner: manager.EventTask, AdminVersion: credential.Version,
		Logger: logger,
	})
	stage = "initialize_transfers"
	transferManager, err = transfers.New(runtimeCtx, transfers.Dependencies{DB: db, ADB: adbClient, Scopes: mediaManager, Logger: logger}, transferConfig)
	if err != nil {
		return errors.Join(err, closeRuntime())
	}
	previewManager = previews.New(runtimeCtx, registry, adbClient)
	registry.Start(manager)
	app := &platform.Server{
		DB: db, Admin: credential, SessionTTL: options.sessionTTL, Logger: logger,
		Leases: manager, Devices: registry, Connections: tunnels, Events: stateEvents, Previews: previewManager,
		Media: mediaManager, Transfers: transferManager,
	}
	server := &http.Server{
		Addr:              options.listen,
		Handler:           app.Handler(static),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}
	stage = "bind_http"
	listener, err := net.Listen("tcp", options.listen)
	if err != nil {
		return errors.Join(err, closeRuntime())
	}
	stage = "running"
	ready()
	result := make(chan error, 1)
	go func() {
		logger.Info("http_listening", "address", listener.Addr().String())
		result <- server.Serve(listener)
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
	stage = "shutdown"
	return errors.Join(err, closeRuntime())
}

func buildInfo() map[string]string {
	result := make(map[string]string)
	if info, ok := debug.ReadBuildInfo(); ok {
		result["go"] = info.GoVersion
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision", "vcs.time", "vcs.modified":
				result[setting.Key] = setting.Value
			}
		}
	}
	return result
}
