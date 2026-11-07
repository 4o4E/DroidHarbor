package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"droidharbor/internal/adbgateway"
)

func runConnect(args []string) (failure *commandFailure) {
	options, failure := parseConnectionFlags("connect", args, true)
	if failure != nil {
		return failure
	}
	adbBinary, failure := ensureAbsoluteADBPath(options.adbBin)
	if failure != nil {
		return failure
	}
	directory, unlock, failure := lockedConnectionDir(options.taskDir)
	if failure != nil {
		return failure
	}
	locked := true
	defer func() {
		if locked {
			if err := unlock(); err != nil {
				failure = appendFailure(failure, "TASK_LOCK_ERROR", "连接目录锁关闭失败", err, 2)
			}
		}
	}()
	task, failure := loadTaskState(directory)
	if failure != nil {
		return failure
	}
	if task.LeaseID == "" {
		return &commandFailure{Code: "LEASE_REQUIRED", Message: "connect 需要已持久化的有效 leaseId；请先显式 claim", exitCode: 2}
	}
	api, failure := newAPI(task.Server, options.caFile)
	if failure != nil {
		return failure
	}
	defer api.client.CloseIdleConnections()
	if failure = requireRemoteADB(api, task); failure != nil {
		return failure
	}
	if failure = validateNativeADBVersion(adbBinary); failure != nil {
		return failure
	}

	state, failure := loadConnectionState(directory, task)
	if failure != nil {
		return failure
	}
	if state != nil && !state.LocalCleanupConfirmed {
		live, identityErr := processIdentityMatches(state.OwnerPID, state.OwnerBirth)
		if identityErr != nil {
			return &commandFailure{Code: "CONNECTION_UNKNOWN", Message: fmt.Sprintf("无法核验现有连接进程身份：%v；不会联系或终止该进程", identityErr), exitCode: 6}
		}
		if live {
			local, controlErr := callLocalControl(state, "status")
			if controlErr != nil || local.Status != "active" {
				message := "已有连接进程仍存活但状态无法核验；不会启动第二个 transport"
				if controlErr != nil {
					message += ": " + controlErr.Error()
				} else {
					message += fmt.Sprintf(" (status=%q)", local.Status)
				}
				return &commandFailure{Code: "CONNECTION_UNKNOWN", Message: message, exitCode: 6}
			}
			return &commandFailure{Code: "CONNECTION_ALREADY_ACTIVE", Message: "此任务已有前台 connect 进程；请复用其 adbArgs 或显式 disconnect", exitCode: 5}
		}
		if failure = confirmExitedConnectionOwner(state); failure != nil {
			return failure
		}
		state.LocalCleanupConfirmed = true
		if failure = saveConnectionState(directory, state); failure != nil {
			return failure
		}
	}

	if state != nil && state.Connection != nil {
		connection, _, requestID, requestFailure := requestRemoteConnection(api, task, http.MethodGet, connectionResourcePath(task.LeaseID, state.Connection.ConnectionID), nil, "")
		if requestFailure != nil {
			return requestFailure
		}
		state.Connection = &connection
		if failure = saveConnectionState(directory, state); failure != nil {
			return failure
		}
		switch connection.Status {
		case "active":
			return &commandFailure{Code: "CONNECTION_USED", Message: "服务端连接已有 transport；不会自动重连或重放", RequestID: requestID, exitCode: 5}
		case "closing":
			return &commandFailure{Code: "CONNECTION_CLOSING", Message: "服务端连接仍在清理；请显式 disconnect 查询，不能建立第二个 transport", RequestID: requestID, exitCode: 5}
		case "closed":
			if err := os.RemoveAll(state.ADBHome); err != nil {
				return &commandFailure{Code: "ADB_HOME_CLEANUP_FAILED", Message: err.Error(), exitCode: 2}
			}
			state = nil
		case "failed":
			return &commandFailure{Code: "CONNECTION_FAILED", Message: "服务端已将此前的连接标记为 failed；请显式 disconnect 后再建立新连接", RequestID: requestID, exitCode: 5}
		case "pending":
		}
	}

	newAttempt := state == nil
	discardLocalAttempt := false
	localCleanupConfirmed := true
	// Keep the original key once creation may have reached the platform.
	// A fresh, unsubmitted or explicitly rejected attempt owns no remote result.
	defer func() {
		if !discardLocalAttempt || !localCleanupConfirmed {
			return
		}
		if err := os.RemoveAll(state.ADBHome); err != nil {
			failure = appendFailure(failure, "ADB_HOME_CLEANUP_FAILED", "无法清除未创建连接的任务 adb 目录", err, 2)
			return
		}
		if err := os.Remove(filepath.Join(directory, connectionStateName)); err != nil && !errors.Is(err, os.ErrNotExist) {
			failure = appendFailure(failure, "CONNECTION_STATE_CLEANUP_FAILED", "无法清除未创建的本地连接记录", err, 2)
		}
	}()
	if state == nil {
		state, failure = createConnectionAttempt(directory, task, adbBinary)
		if failure != nil {
			return failure
		}
		discardLocalAttempt = true
		if failure = saveConnectionState(directory, state); failure != nil {
			return failure
		}
	} else {
		if state.Connection != nil && state.Connection.Status != "pending" {
			return &commandFailure{Code: "CONNECTION_NOT_PENDING", Message: "此前连接不是可启动的 pending 状态", exitCode: 5}
		}
		state.ADBPath = adbBinary
		if failure = verifySavedADBKey(state); failure != nil {
			return failure
		}
		if failure = saveConnectionState(directory, state); failure != nil {
			return failure
		}
	}
	adbEnv := connectionADBEnvironment(state.ADBHome, state.ADBKeyPath)

	virtualListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return &commandFailure{Code: "LOCAL_LISTENER_ERROR", Message: err.Error(), exitCode: 5}
	}
	serverPort, err := reserveLoopbackPort()
	if err != nil {
		_ = virtualListener.Close()
		return &commandFailure{Code: "LOCAL_LISTENER_ERROR", Message: err.Error(), exitCode: 5}
	}
	controlSecret, err := randomHex(32)
	if err != nil {
		_ = virtualListener.Close()
		return &commandFailure{Code: "RANDOM_FAILED", Message: err.Error(), exitCode: 2}
	}
	ownerBirth, err := processBirthIdentity(os.Getpid())
	if err != nil {
		_ = virtualListener.Close()
		return &commandFailure{Code: "PROCESS_IDENTITY_ERROR", Message: err.Error(), exitCode: 5}
	}
	state.OwnerPID = os.Getpid()
	state.OwnerBirth = ownerBirth
	state.ControlSecret = controlSecret
	state.ADBServerPort = serverPort
	state.DeviceEndpoint = virtualListener.Addr().String()
	state.LocalCleanupConfirmed = false
	control, err := startLocalControl(directory, task, state, adbEnv)
	if err != nil {
		_ = virtualListener.Close()
		state.LocalCleanupConfirmed = true
		return &commandFailure{Code: "LOCAL_CONTROL_ERROR", Message: err.Error(), exitCode: 5}
	}
	state.ControlAddress = control.state.ControlAddress
	resources := &connectResources{directory: directory, task: task, state: state, api: api, control: control, virtualListener: virtualListener}
	defer func() {
		cleanupFailure := resources.cleanup()
		if cleanupFailure != nil {
			localCleanupConfirmed = false
			if failure == nil {
				failure = cleanupFailure
			} else {
				if failure.RequestID == "" {
					failure.RequestID = cleanupFailure.RequestID
				}
				failure.Message = failure.Message + "；清理错误：" + cleanupFailure.Message
			}
		}
	}()

	rootContext, cancelRoot := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	resources.cancelRoot = cancelRoot

	adbArgs := []string{"-L", fmt.Sprintf("tcp:%d", serverPort), "--one-device", state.OneDeviceSerial, "nodaemon", "server"}
	server, err := startOwnedADBServer(adbBinary, adbArgs, adbEnv)
	if err != nil {
		return &commandFailure{Code: "ADB_SERVER_START_FAILED", Message: err.Error(), exitCode: 5}
	}
	resources.server = server
	control.setADBServer(server)
	state.ADBServerPID = int(server.pid)
	state.ADBServerBirth = server.born
	if failure = waitForNativeADBServer(server, adbBinary, adbEnv, serverPort, state.ADBHome); failure != nil {
		return failure
	}
	if failure = saveConnectionState(directory, state); failure != nil {
		return failure
	}

	if state.Connection == nil {
		body, bodyErr := connectionAttemptBody(state)
		if bodyErr != nil {
			return &commandFailure{Code: "CONNECTION_STATE_ERROR", Message: bodyErr.Error(), exitCode: 2}
		}
		discardLocalAttempt = false
		connection, status, requestID, requestFailure := requestRemoteConnection(api, task, http.MethodPost, "/leases/"+url.PathEscape(task.LeaseID)+"/connections", body, state.IdempotencyKey)
		if requestFailure != nil {
			discardLocalAttempt = newAttempt && status >= 400 && status < 500 && requestFailure.exitCode != 6
			return requestFailure
		}
		state.Connection = &connection
		control.setConnection(connection)
		if failure = saveConnectionState(directory, state); failure != nil {
			return &commandFailure{Code: "RESULT_UNKNOWN", Message: "服务端已返回 connectionId，但连接状态持久化失败；保留原幂等键以供显式恢复", RequestID: requestID, exitCode: 6}
		}
	}
	if state.Connection.Status != "pending" {
		return &commandFailure{Code: "CONNECTION_NOT_PENDING", Message: "只有 pending 连接允许建立首次 transport", exitCode: 5}
	}
	connection, _, requestID, requestFailure := requestRemoteConnection(api, task, http.MethodGet, connectionResourcePath(task.LeaseID, state.Connection.ConnectionID), nil, "")
	if requestFailure != nil {
		return requestFailure
	}
	if connection.Status != "pending" {
		if connection.Status == "closed" || connection.Status == "failed" {
			return &commandFailure{Code: "CONNECTION_NOT_PENDING", Message: "服务端连接已离开 pending 状态；不会建立第二个 transport", RequestID: requestID, exitCode: 5}
		}
		if connection.Status == "closing" {
			return &commandFailure{Code: "CONNECTION_CLOSING", Message: "服务端连接仍在清理；不会建立第二个 transport", RequestID: requestID, exitCode: 5}
		}
		return &commandFailure{Code: "CONNECTION_USED", Message: "服务端连接已有 transport；不会自动重连或重放", RequestID: requestID, exitCode: 5}
	}

	tunnel, handshakeFailure := dialConnectionTunnel(rootContext, api, connection, task.TaskSecret)
	if handshakeFailure != nil {
		return handshakeFailure
	}
	resources.tunnel = tunnel
	resources.deleteRemoteOnCleanup = true
	startupDeadline := time.NewTimer(30 * time.Second)
	defer startupDeadline.Stop()
	accepted := make(chan net.Conn, 1)
	tunnelResult := make(chan error, 1)
	tunnelWorkerDone := make(chan struct{})
	resources.tunnelWorkerDone = tunnelWorkerDone
	go func() {
		defer close(tunnelWorkerDone)
		local, acceptErr := virtualListener.Accept()
		if acceptErr != nil {
			tunnelResult <- acceptErr
			return
		}
		accepted <- local
		_ = virtualListener.Close()
		bridgeErr := bridgeConnection(rootContext, local, tunnel)
		control.setReady(false)
		tunnelResult <- bridgeErr
	}()

	connectResult := make(chan nativeCommandResult, 1)
	nativeWorkerDone := make(chan struct{})
	resources.nativeWorkerDone = nativeWorkerDone
	go func() {
		defer close(nativeWorkerDone)
		ctx, cancel := context.WithTimeout(rootContext, 30*time.Second)
		defer cancel()
		output, connectErr := runNativeADB(ctx, adbBinary, adbEnv, serverPort, []string{"connect", state.DeviceEndpoint})
		connectResult <- nativeCommandResult{output: output, err: connectErr}
	}()
	var connectOutput []byte
	select {
	case <-rootContext.Done():
		return &commandFailure{Code: "CONNECTION_INTERRUPTED", Message: "连接建立已中断", exitCode: 4}
	case <-startupDeadline.C:
		return &commandFailure{Code: "ADB_CONNECT_TIMEOUT", Message: "原生 ADB 建连超时", exitCode: 4}
	case result := <-connectResult:
		connectOutput = result.output
		if result.err != nil {
			return &commandFailure{Code: "ADB_CONNECT_FAILED", Message: result.err.Error(), exitCode: 5}
		}
	case tunnelErr := <-tunnelResult:
		return &commandFailure{Code: "ADB_TUNNEL_LOST", Message: "WSS/loopback 通道在原生 adb connect 完成前关闭：" + fmt.Sprint(tunnelErr), exitCode: 4}
	case <-server.done:
		return &commandFailure{Code: "ADB_SERVER_EXITED", Message: fmt.Sprintf("本地 adb Server 在 connect 期间退出：%v", server.wait()), exitCode: 5}
	case controlErr := <-control.failureCh:
		return &commandFailure{Code: "LOCAL_CONTROL_FAILED", Message: controlErr.Error(), exitCode: 5}
	}
	select {
	case <-rootContext.Done():
		return &commandFailure{Code: "CONNECTION_INTERRUPTED", Message: "连接建立已中断", exitCode: 4}
	case <-startupDeadline.C:
		return &commandFailure{Code: "ADB_CONNECT_TIMEOUT", Message: "原生 ADB 未连接任务入口", exitCode: 4}
	case resources.localConn = <-accepted:
	case tunnelErr := <-tunnelResult:
		return &commandFailure{Code: "ADB_TUNNEL_LOST", Message: "WSS/loopback 通道在原生 adb connect 后关闭：" + fmt.Sprint(tunnelErr), exitCode: 4}
	case <-server.done:
		return &commandFailure{Code: "ADB_SERVER_EXITED", Message: fmt.Sprintf("本地 adb Server 在 connect 后退出：%v", server.wait()), exitCode: 5}
	case controlErr := <-control.failureCh:
		return &commandFailure{Code: "LOCAL_CONTROL_FAILED", Message: controlErr.Error(), exitCode: 5}
	}

	stateContext, cancelState := context.WithTimeout(rootContext, 10*time.Second)
	stateOutput, stateErr := runNativeADB(stateContext, adbBinary, adbEnv, serverPort, []string{"-s", state.DeviceEndpoint, "get-state"})
	cancelState()
	if stateErr != nil || strings.TrimSpace(string(stateOutput)) != "device" {
		message := fmt.Sprintf("原生 adb get-state 未确认 device（connect 输出：%s）", strings.TrimSpace(string(connectOutput)))
		if stateErr != nil {
			message += ": " + stateErr.Error()
		}
		return &commandFailure{Code: "ADB_DEVICE_NOT_READY", Message: message, exitCode: 5}
	}
	connection, _, requestID, requestFailure = requestRemoteConnection(api, task, http.MethodGet, connectionResourcePath(task.LeaseID, state.Connection.ConnectionID), nil, "")
	if requestFailure != nil {
		return requestFailure
	}
	if connection.Status != "active" {
		return &commandFailure{Code: "CONNECTION_NOT_ACTIVE", Message: "原生 adb 已报告 device，但平台连接尚未报告 active", RequestID: requestID, exitCode: 5}
	}
	state.Connection = &connection
	control.setConnection(connection)
	if failure = saveConnectionState(directory, state); failure != nil {
		return failure
	}
	control.setReady(true)
	ready := publicEnvironment(state, connection)
	if err = json.NewEncoder(os.Stdout).Encode(ready); err != nil {
		control.setReady(false)
		return &commandFailure{Code: "OUTPUT_FAILED", Message: err.Error(), exitCode: 1}
	}
	if err = unlock(); err != nil {
		locked = false
		return &commandFailure{Code: "TASK_LOCK_ERROR", Message: "ready 已输出，但连接目录锁无法关闭：" + err.Error(), exitCode: 2}
	}
	locked = false

	select {
	case <-rootContext.Done():
		failure = &commandFailure{Code: "CONNECTION_INTERRUPTED", Message: "连接已中断", exitCode: 4}
	case <-control.stopCh:
		resources.normalStop = true
	case tunnelErr := <-tunnelResult:
		select {
		case <-control.stopCh:
			resources.normalStop = true
		default:
			if tunnelErr == nil {
				tunnelErr = errors.New("ADB tunnel closed")
			}
			failure = &commandFailure{Code: "ADB_TUNNEL_LOST", Message: tunnelErr.Error(), exitCode: 4}
		}
	case <-server.done:
		select {
		case <-control.stopCh:
			resources.normalStop = true
		default:
			failure = &commandFailure{Code: "ADB_SERVER_EXITED", Message: fmt.Sprintf("本地 adb Server 已退出：%v", server.wait()), exitCode: 5}
		}
	case controlErr := <-control.failureCh:
		failure = &commandFailure{Code: "LOCAL_CONTROL_FAILED", Message: controlErr.Error(), exitCode: 5}
	}
	return failure
}

type nativeCommandResult struct {
	output []byte
	err    error
}

type connectResources struct {
	directory             string
	task                  *taskState
	state                 *connectionState
	api                   *apiClient
	control               *connectionRuntime
	virtualListener       net.Listener
	localConn             net.Conn
	server                *ownedADBServer
	tunnel                interface{ CloseNow() error }
	tunnelWorkerDone      <-chan struct{}
	nativeWorkerDone      <-chan struct{}
	cancelRoot            context.CancelFunc
	deleteRemoteOnCleanup bool
	normalStop            bool
	cleaned               bool
}

func (r *connectResources) cleanup() *commandFailure {
	if r.cleaned {
		return nil
	}
	r.cleaned = true
	var failures []error
	if r.control != nil {
		r.control.requestStop()
	}
	if r.cancelRoot != nil {
		r.cancelRoot()
	}
	if r.tunnel != nil {
		if err := r.tunnel.CloseNow(); err != nil && !errors.Is(err, net.ErrClosed) {
			failures = append(failures, err)
		}
	}
	if r.localConn != nil {
		if err := r.localConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			failures = append(failures, err)
		}
	}
	if r.virtualListener != nil {
		if err := r.virtualListener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			failures = append(failures, err)
		}
	}
	if r.nativeWorkerDone != nil {
		<-r.nativeWorkerDone
	}
	if r.tunnelWorkerDone != nil {
		<-r.tunnelWorkerDone
	}
	if r.server != nil {
		if err := r.server.stop(); err != nil {
			failures = append(failures, err)
		}
	}
	if r.control != nil {
		if err := stopLocalControl(r.control); err != nil {
			failures = append(failures, err)
		}
	}
	cleanupErr := errors.Join(failures...)
	if cleanupErr == nil {
		r.state.LocalCleanupConfirmed = true
	}
	var cleanupFailure *commandFailure
	if cleanupErr == nil && r.deleteRemoteOnCleanup && r.state.Connection != nil && !r.normalStop && r.state.Connection.Status != "closed" {
		connection, _, requestID, deleteFailure := requestRemoteConnection(r.api, r.task, http.MethodDelete, connectionResourcePath(r.task.LeaseID, r.state.Connection.ConnectionID), nil, "")
		if deleteFailure != nil {
			cleanupFailure = deleteFailure
		} else {
			r.state.Connection = &connection
			if connection.Status != "closed" {
				code, message := "RESULT_UNKNOWN", "清理 DELETE 未返回 confirmed closed"
				if connection.Status == "closing" {
					code, message = "CONNECTION_CLOSING", "平台报告连接仍在 closing；该状态不是 confirmed closed"
				}
				cleanupFailure = &commandFailure{Code: code, Message: message, RequestID: requestID, exitCode: 6}
			}
		}
	}
	if failure := saveConnectionState(r.directory, r.state); failure != nil {
		if cleanupFailure != nil {
			cleanupFailure.Message += "；无法保存清理状态：" + failure.Message
		} else if cleanupErr != nil {
			cleanupErr = errors.Join(cleanupErr, errors.New(failure.Message))
		} else {
			return failure
		}
	}
	if cleanupFailure != nil {
		if cleanupErr != nil {
			cleanupFailure.Message += "；本地清理错误：" + cleanupErr.Error()
		}
		return cleanupFailure
	}
	if cleanupErr != nil {
		return &commandFailure{Code: "LOCAL_CLEANUP_FAILED", Message: "本地连接清理未全部确认：" + cleanupErr.Error(), exitCode: 5}
	}
	return nil
}

func envCommand(args []string) (result json.RawMessage, failure *commandFailure) {
	options, failure := parseConnectionFlags("env", args, false)
	if failure != nil {
		return nil, failure
	}
	directory, unlock, failure := lockedConnectionDir(options.taskDir)
	if failure != nil {
		return nil, failure
	}
	locked := true
	defer func() {
		if locked {
			if err := unlock(); err != nil {
				result = nil
				failure = appendFailure(failure, "TASK_LOCK_ERROR", "连接目录锁关闭失败", err, 2)
			}
		}
	}()
	task, failure := loadTaskState(directory)
	if failure != nil {
		return nil, failure
	}
	connectionState, failure := loadConnectionState(directory, task)
	if failure != nil {
		return nil, failure
	}
	if connectionState == nil || connectionState.Connection == nil || connectionState.LocalCleanupConfirmed {
		return nil, &commandFailure{Code: "CONNECTION_NOT_ACTIVE", Message: "当前没有经验证的活动连接；不会返回已保存端口", exitCode: 5}
	}
	live, err := processIdentityMatches(connectionState.OwnerPID, connectionState.OwnerBirth)
	if err != nil || !live {
		message := "无法核验连接进程身份；不会联系已保存的本地 control 地址"
		if err != nil {
			message += ": " + err.Error()
		}
		return nil, &commandFailure{Code: "CONNECTION_UNKNOWN", Message: message, exitCode: 6}
	}
	local, err := callLocalControl(connectionState, "status")
	if err != nil || local.Status != "active" || local.Environment == nil {
		message := "本地连接或原生 adb 未确认活动状态"
		if err != nil {
			message += ": " + err.Error()
		} else if local.Status != "" {
			message += fmt.Sprintf(" (status=%q)", local.Status)
		}
		return nil, &commandFailure{Code: "CONNECTION_NOT_ACTIVE", Message: message, exitCode: 5}
	}
	api, failure := newAPI(task.Server, options.caFile)
	if failure != nil {
		return nil, failure
	}
	defer api.client.CloseIdleConnections()
	remote, _, requestID, failure := requestRemoteConnection(api, task, http.MethodGet, connectionResourcePath(task.LeaseID, connectionState.Connection.ConnectionID), nil, "")
	if failure != nil {
		return nil, failure
	}
	environment := local.Environment
	if remote.Status != "active" || environment.ConnectionID != remote.ConnectionID || environment.LeaseID != remote.LeaseID ||
		environment.DeviceID != remote.DeviceID || environment.Generation != remote.Generation || environment.BootID != remote.BootID {
		return nil, &commandFailure{Code: "CONNECTION_NOT_ACTIVE", Message: "平台与本地 control 未共同确认同一 active 连接", RequestID: requestID, exitCode: 5}
	}
	// Renewal changes the authoritative expiry without restarting the transport.
	environment.ExpiresAt = remote.ExpiresAt
	encoded, err := json.Marshal(local.Environment)
	if err != nil {
		return nil, &commandFailure{Code: "OUTPUT_FAILED", Message: err.Error(), exitCode: 1}
	}
	return encoded, nil
}

func disconnectCommand(args []string) (result json.RawMessage, failure *commandFailure) {
	options, failure := parseConnectionFlags("disconnect", args, false)
	if failure != nil {
		return nil, failure
	}
	directory, unlock, failure := lockedConnectionDir(options.taskDir)
	if failure != nil {
		return nil, failure
	}
	locked := true
	defer func() {
		if locked {
			if err := unlock(); err != nil {
				result = nil
				failure = appendFailure(failure, "TASK_LOCK_ERROR", "连接目录锁关闭失败", err, 2)
			}
		}
	}()
	task, failure := loadTaskState(directory)
	if failure != nil {
		return nil, failure
	}
	state, failure := loadConnectionState(directory, task)
	if failure != nil {
		return nil, failure
	}
	if state == nil {
		return nil, &commandFailure{Code: "CONNECTION_STATE_MISSING", Message: "尚无本地连接尝试记录", exitCode: 2}
	}
	if state.Connection != nil && state.Connection.Status == "closed" && state.LocalCleanupConfirmed {
		return json.RawMessage(fmt.Sprintf(`{"connectionId":%q,"status":"closed"}`, state.Connection.ConnectionID)), nil
	}
	if !state.LocalCleanupConfirmed {
		live, identityErr := processIdentityMatches(state.OwnerPID, state.OwnerBirth)
		if identityErr != nil {
			return nil, &commandFailure{Code: "CONNECTION_UNKNOWN", Message: fmt.Sprintf("无法核验原连接进程：%v；不会联系 control 地址或按旧 PID/端口清理", identityErr), exitCode: 6}
		}
		if live {
			if state.Connection == nil {
				return nil, &commandFailure{Code: "RESULT_UNKNOWN", Message: "连接创建结果未知且尚无 connectionId；请显式 connect 以复用原幂等键恢复结果", exitCode: 6}
			}
			local, err := callLocalControl(state, "stop")
			if err != nil || local.Status != "stopping" {
				message := "本地连接进程未确认停止请求；未发送远端 DELETE"
				if err != nil {
					message += ": " + err.Error()
				} else {
					message += fmt.Sprintf(" (status=%q)", local.Status)
				}
				return nil, &commandFailure{Code: "LOCAL_CLEANUP_UNKNOWN", Message: message, exitCode: 6}
			}
			if failure = waitForConnectionExit(state, 30*time.Second); failure != nil {
				return nil, failure
			}
			// The foreground writer has exited; it cannot race this final update.
			refreshed, readFailure := loadConnectionState(directory, task)
			if readFailure != nil {
				return nil, readFailure
			}
			if refreshed == nil || refreshed.Connection == nil || refreshed.Connection.ConnectionID != state.Connection.ConnectionID {
				return nil, &commandFailure{Code: "CONNECTION_STATE_CHANGED", Message: "停止后连接记录已改变，拒绝覆盖", exitCode: 6}
			}
			state = refreshed
		} else if failure = confirmExitedConnectionOwner(state); failure != nil {
			return nil, failure
		}
		state.LocalCleanupConfirmed = true
		if failure = saveConnectionState(directory, state); failure != nil {
			return nil, failure
		}
	}
	if state.Connection == nil {
		return nil, &commandFailure{Code: "RESULT_UNKNOWN", Message: "连接创建结果未知且尚无 connectionId；请显式 connect 以复用原幂等键恢复结果", exitCode: 6}
	}
	api, failure := newAPI(task.Server, options.caFile)
	if failure != nil {
		return nil, failure
	}
	defer api.client.CloseIdleConnections()
	remote, _, requestID, remoteFailure := requestRemoteConnection(api, task, http.MethodDelete, connectionResourcePath(task.LeaseID, state.Connection.ConnectionID), nil, "")
	if remoteFailure != nil {
		return nil, remoteFailure
	}
	state.Connection = &remote
	if failure = saveConnectionState(directory, state); failure != nil {
		failure.RequestID = requestID
		return nil, failure
	}
	if remote.Status != "closed" {
		code, message := "RESULT_UNKNOWN", "DELETE 未返回 confirmed closed"
		if remote.Status == "closing" {
			code, message = "CONNECTION_CLOSING", "平台报告连接仍在 closing；该状态不是 confirmed closed"
		}
		return nil, &commandFailure{Code: code, Message: message + "；请显式查询连接状态", RequestID: requestID, exitCode: 6}
	}
	encoded, err := json.Marshal(struct {
		ConnectionID string `json:"connectionId"`
		Status       string `json:"status"`
	}{ConnectionID: remote.ConnectionID, Status: "closed"})
	if err != nil {
		return nil, &commandFailure{Code: "OUTPUT_FAILED", Message: err.Error(), exitCode: 1}
	}
	return encoded, nil
}

func requireRemoteADB(api *apiClient, task *taskState) *commandFailure {
	data, status, requestID, err := api.do(http.MethodGet, "/capabilities", nil, map[string]string{"Authorization": "Bearer " + task.TaskSecret})
	if err != nil {
		return &commandFailure{Code: "CONNECTION_FAILED", Message: err.Error(), RequestID: requestID, exitCode: 4}
	}
	if status != http.StatusOK {
		return httpFailure(data, status, requestID)
	}
	var capabilities struct {
		APIMajor int             `json:"apiMajor"`
		Features map[string]bool `json:"features"`
	}
	if json.Unmarshal(data, &capabilities) != nil || capabilities.Features == nil {
		return &commandFailure{Code: "INVALID_RESPONSE", Message: "平台未返回有效能力对象", RequestID: requestID, exitCode: 4}
	}
	if capabilities.APIMajor != 1 {
		return &commandFailure{Code: "API_INCOMPATIBLE", Message: fmt.Sprintf("CLI 要求 API major 1，平台返回 %d", capabilities.APIMajor), RequestID: requestID, exitCode: 5}
	}
	if !capabilities.Features["remoteAdb"] {
		return &commandFailure{Code: "CAPABILITY_UNAVAILABLE", Message: "平台未声明 remoteAdb 能力；未提交连接创建请求", RequestID: requestID, exitCode: 5}
	}
	return nil
}

func validateNativeADBVersion(binary string) *commandFailure {
	return validateNativeADBVersionWithEnvironment(binary, os.Environ())
}

func validateNativeADBVersionWithEnvironment(binary string, environment []string) *commandFailure {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "version")
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		return &commandFailure{Code: "ADB_BINARY_ERROR", Message: fmt.Sprintf("无法读取 adb 实际版本：%v: %s", err, strings.TrimSpace(string(output))), exitCode: 5}
	}
	text := string(output)
	if !strings.Contains(text, "Android Debug Bridge version 1.0.41") || !hasLockedPlatformToolsVersion(text) {
		return &commandFailure{Code: "ADB_VERSION_INCOMPATIBLE", Message: "--adb-bin 必须报告锁定的 Android Debug Bridge 1.0.41 / platform-tools 37.0.1；不会下载或升级", exitCode: 5}
	}
	return nil
}

func hasLockedPlatformToolsVersion(versionOutput string) bool {
	for _, line := range strings.Split(versionOutput, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Version ") {
			version := strings.TrimPrefix(line, "Version ")
			return version == "37.0.1" || strings.HasPrefix(version, "37.0.1-") || strings.HasPrefix(version, "37.0.1 ")
		}
	}
	return false
}

func createConnectionAttempt(directory string, task *taskState, adbBinary string) (state *connectionState, failure *commandFailure) {
	opaque, err := randomHex(16)
	if err != nil {
		return nil, &commandFailure{Code: "RANDOM_FAILED", Message: err.Error(), exitCode: 2}
	}
	idempotencyKey, err := randomHex(32)
	if err != nil {
		return nil, &commandFailure{Code: "RANDOM_FAILED", Message: err.Error(), exitCode: 2}
	}
	home := filepath.Join(directory, ".droidharbor-adb-"+opaque)
	homeCreated := false
	defer func() {
		if homeCreated && state == nil {
			if cleanupErr := os.RemoveAll(home); cleanupErr != nil {
				failure = appendFailure(failure, "ADB_HOME_CLEANUP_FAILED", "无法清除未使用的任务 adb 目录", cleanupErr, 2)
			}
		}
	}()
	if err = createPrivateDirectory(home); err != nil {
		return nil, &commandFailure{Code: "ADB_HOME_ERROR", Message: err.Error(), exitCode: 2}
	}
	homeCreated = true
	keyDirectory := filepath.Join(home, ".android")
	if err = createPrivateDirectory(keyDirectory); err != nil {
		return nil, &commandFailure{Code: "ADB_HOME_ERROR", Message: err.Error(), exitCode: 2}
	}
	keyPath := filepath.Join(keyDirectory, "adbkey")
	environment := connectionADBEnvironment(home, keyPath)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	keygen := exec.CommandContext(ctx, adbBinary, "keygen", keyPath)
	keygen.Env = environment
	output, err := keygen.CombinedOutput()
	if err != nil {
		return nil, &commandFailure{Code: "ADB_KEYGEN_FAILED", Message: fmt.Sprintf("原生 adb keygen 失败：%v: %s", err, strings.TrimSpace(string(output))), exitCode: 5}
	}
	publicKey, failure := readStandardADBPublicKey(keyPath + ".pub")
	if failure != nil {
		return nil, failure
	}
	// SDK keygen creates the public file with its own default mode. Both files
	// belong to this newly created task directory, so establish our permissions
	// before applying the strict checks used for previously saved task files.
	for _, path := range []string{keyPath, keyPath + ".pub"} {
		if err = os.Chmod(path, 0600); err != nil {
			return nil, &commandFailure{Code: "ADB_KEY_PERMISSIONS", Message: err.Error(), exitCode: 2}
		}
		if err = protectPrivatePath(path); err != nil {
			return nil, &commandFailure{Code: "ADB_KEY_PERMISSIONS", Message: err.Error(), exitCode: 2}
		}
	}
	return &connectionState{
		TaskID: task.TaskID, LeaseID: task.LeaseID, IdempotencyKey: idempotencyKey, ProtocolMajor: 1,
		ADBPublicKey: publicKey, ADBHome: home, ADBKeyPath: keyPath, ADBPath: adbBinary,
		OneDeviceSerial: "DroidHarbor-Task-" + opaque, LocalCleanupConfirmed: true,
	}, nil
}

func readStandardADBPublicKey(path string) (string, *commandFailure) {
	file, err := os.Open(path)
	if err != nil {
		return "", &commandFailure{Code: "ADB_KEYGEN_FAILED", Message: err.Error(), exitCode: 5}
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 65537))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > 65536 {
		return "", &commandFailure{Code: "ADB_KEYGEN_FAILED", Message: errors.Join(readErr, closeErr, errors.New("adb 公钥读取不完整或文件超过 64 KiB")).Error(), exitCode: 5}
	}
	key := strings.TrimSpace(string(data))
	fields := strings.Fields(key)
	if strings.ContainsAny(key, "\r\n") || len(fields) < 2 {
		return "", &commandFailure{Code: "ADB_KEYGEN_FAILED", Message: "adb keygen 未生成单行标准 adb .pub 公钥", exitCode: 5}
	}
	if _, err = adbgateway.ParsePublicKey(key); err != nil {
		return "", &commandFailure{Code: "ADB_KEYGEN_FAILED", Message: "adb keygen 未生成受支持的标准 ADB 公钥", exitCode: 5}
	}
	return key, nil
}

func verifySavedADBKey(state *connectionState) *commandFailure {
	if err := secureTaskDirectory(state.ADBHome); err != nil {
		return &commandFailure{Code: "ADB_HOME_ERROR", Message: err.Error(), exitCode: 2}
	}
	privateInfo, err := os.Stat(state.ADBKeyPath)
	if err != nil || !privateInfo.Mode().IsRegular() {
		if err == nil {
			err = errors.New("任务 adb 私钥路径不是普通文件")
		}
		return &commandFailure{Code: "CONNECTION_STATE_INVALID", Message: "保存的 adb 私钥不可用；不会生成替代密钥", exitCode: 2}
	}
	if err = protectPrivatePath(state.ADBKeyPath); err != nil {
		return &commandFailure{Code: "ADB_KEY_PERMISSIONS", Message: err.Error(), exitCode: 2}
	}
	publicKey, failure := readStandardADBPublicKey(state.ADBKeyPath + ".pub")
	if failure != nil || publicKey != state.ADBPublicKey {
		return &commandFailure{Code: "CONNECTION_STATE_INVALID", Message: "保存的公钥与原幂等请求不一致；不会替换密钥", exitCode: 2}
	}
	return nil
}

func connectionADBEnvironment(home, keyPath string) []string {
	blocked := map[string]bool{
		"ADB_SERVER_SOCKET": true, "ADB_SERVER_PORT": true, "ANDROID_SERIAL": true,
		"ANDROID_ADB_SERVER_PORT": true, "ANDROID_ADB_LOG_PATH": true, "ADB_TRACE": true,
		"ADB_VENDOR_KEYS": true, "ANDROID_USER_HOME": true, "ANDROID_SDK_HOME": true,
		"HOME": true, "USERPROFILE": true, "ADB_USB": true, "ADB_EMU": true,
		"ADB_MDNS": true, "ADB_MDNS_AUTO_CONNECT": true, "ADB_LOCAL_TRANSPORT_MAX_PORT": true,
	}
	canonical := func(value string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(value)
		}
		return value
	}
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if !found || key == "" {
			continue
		}
		if blocked[canonical(key)] {
			continue
		}
		values[canonical(key)] = entry
	}
	fixed := map[string]string{
		"ANDROID_USER_HOME": home, "ANDROID_SDK_HOME": home, "ADB_VENDOR_KEYS": keyPath,
		"HOME": home, "USERPROFILE": home, "ADB_USB": "0", "ADB_EMU": "0",
		"ADB_MDNS": "0", "ADB_MDNS_AUTO_CONNECT": "0", "ADB_LOCAL_TRANSPORT_MAX_PORT": "0",
		"ANDROID_ADB_LOG_PATH": filepath.Join(home, "adb.log"),
	}
	for key, value := range fixed {
		values[canonical(key)] = key + "=" + value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, values[key])
	}
	return environment
}

func reserveLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitForNativeADBServer(server *ownedADBServer, binary string, environment []string, port int, home string) *commandFailure {
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-server.done:
			return &commandFailure{Code: "ADB_SERVER_START_FAILED", Message: fmt.Sprintf("原生 adb Server 已退出：%v", server.wait()), exitCode: 5}
		default:
		}
		version, err := probeADBServerVersion(port)
		if err == nil {
			if version != "41" {
				return &commandFailure{Code: "ADB_VERSION_INCOMPATIBLE", Message: fmt.Sprintf("本地 adb Server 协议版本为 %s，锁定版本要求 41", version), exitCode: 5}
			}
			output, statusErr := runNativeADBWithTimeout(binary, environment, port, []string{"server-status"}, 3*time.Second)
			if statusErr != nil {
				return &commandFailure{Code: "ADB_SERVER_IDENTITY_ERROR", Message: statusErr.Error(), exitCode: 5}
			}
			if !serverLogMatches(output, filepath.Join(home, "adb.log")) {
				return &commandFailure{Code: "ADB_SERVER_IDENTITY_ERROR", Message: "监听端口不属于本任务配置的 ADB Server，拒绝建连", exitCode: 5}
			}
			return nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return &commandFailure{Code: "ADB_SERVER_START_FAILED", Message: fmt.Sprintf("等待任务专用 adb Server 超时：%v", lastErr), exitCode: 5}
}

func probeADBServerVersion(port int) (string, error) {
	connection, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err = connection.Write([]byte("000chost:version")); err != nil {
		return "", err
	}
	var status [4]byte
	if _, err = io.ReadFull(connection, status[:]); err != nil {
		return "", err
	}
	if string(status[:]) != "OKAY" && string(status[:]) != "FAIL" {
		return "", fmt.Errorf("本地 adb Server 返回无效的 smart-socket 状态 %q", status)
	}
	var header [4]byte
	if _, err = io.ReadFull(connection, header[:]); err != nil {
		return "", err
	}
	length, err := strconv.ParseUint(string(header[:]), 16, 16)
	if err != nil || length == 0 || length > 128 {
		return "", errors.New("本地 adb Server 返回无效的 smart-socket 长度")
	}
	response := make([]byte, int(length))
	if _, err = io.ReadFull(connection, response); err != nil {
		return "", err
	}
	if string(status[:]) == "FAIL" {
		return "", fmt.Errorf("本地 adb Server 拒绝 host:version：%s", response)
	}
	version, err := strconv.ParseUint(string(response), 16, 32)
	if err != nil {
		return "", fmt.Errorf("本地 adb Server 返回无效协议版本 %q", response)
	}
	return strconv.FormatUint(version, 10), nil
}

func runNativeADBWithTimeout(binary string, environment []string, serverPort int, args []string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return runNativeADB(ctx, binary, environment, serverPort, args)
}

func confirmExitedConnectionOwner(state *connectionState) *commandFailure {
	if state.OwnerPID <= 0 || state.OwnerBirth == "" || state.ADBServerPID <= 0 || state.ADBServerBirth == "" {
		return &commandFailure{Code: "CONNECTION_UNKNOWN", Message: "进程身份记录不完整；不会根据旧 PID 或端口清理资源", exitCode: 6}
	}
	ownerLive, ownerErr := processIdentityMatches(state.OwnerPID, state.OwnerBirth)
	serverLive, serverErr := processIdentityMatches(state.ADBServerPID, state.ADBServerBirth)
	if ownerErr != nil || ownerLive || serverErr != nil || serverLive {
		return &commandFailure{Code: "CONNECTION_UNKNOWN", Message: "原 connect 或其任务 adb Server 仍存活或身份无法核验；不会按旧 PID 清理，也不会启动第二个 transport", exitCode: 6}
	}
	return nil
}

func waitForConnectionExit(state *connectionState, timeout time.Duration) *commandFailure {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		ownerLive, ownerErr := processIdentityMatches(state.OwnerPID, state.OwnerBirth)
		serverLive, serverErr := processIdentityMatches(state.ADBServerPID, state.ADBServerBirth)
		if ownerErr != nil || serverErr != nil {
			return &commandFailure{Code: "LOCAL_CLEANUP_UNKNOWN", Message: "无法核验连接进程的退出状态，未按 PID 清理", exitCode: 6}
		}
		if !ownerLive && !serverLive {
			return nil
		}
		select {
		case <-deadline.C:
			return &commandFailure{Code: "LOCAL_CLEANUP_UNKNOWN", Message: "等待本任务连接和 ADB Server 退出超时", exitCode: 6}
		case <-ticker.C:
		}
	}
}

// The native status carries the configured, task-unique log path. This detects
// accidental port collisions without sending connect to an unrelated Server.
func serverLogMatches(output []byte, expected string) bool {
	for _, line := range strings.Split(string(output), "\n") {
		value, found := strings.CutPrefix(strings.TrimSpace(line), "log_absolute_path:")
		if !found {
			continue
		}
		actual, err := strconv.Unquote(strings.TrimSpace(value))
		if err != nil {
			return false
		}
		if runtime.GOOS == "windows" {
			return strings.EqualFold(filepath.Clean(actual), filepath.Clean(expected))
		}
		return filepath.Clean(actual) == filepath.Clean(expected)
	}
	return false
}

func appendFailure(failure *commandFailure, code, context string, extra error, exitCode int) *commandFailure {
	if extra == nil {
		return failure
	}
	if failure == nil {
		return &commandFailure{Code: code, Message: context + ": " + extra.Error(), exitCode: exitCode}
	}
	failure.Message = failure.Message + "；" + context + ": " + extra.Error()
	return failure
}
