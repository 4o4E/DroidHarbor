package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const connectionStateName = "connection-state.json"

type remoteConnection struct {
	ConnectionID  string   `json:"connectionId"`
	LeaseID       string   `json:"leaseId"`
	DeviceID      string   `json:"deviceId"`
	Generation    int64    `json:"generation"`
	BootID        string   `json:"bootId"`
	ProtocolMajor int      `json:"protocolMajor"`
	Status        string   `json:"status"`
	ExpiresAt     string   `json:"expiresAt"`
	Features      []string `json:"features"`
	TunnelPath    string   `json:"tunnelPath"`
}

type connectionState struct {
	TaskID                string            `json:"taskId"`
	LeaseID               string            `json:"leaseId"`
	IdempotencyKey        string            `json:"idempotencyKey"`
	ProtocolMajor         int               `json:"protocolMajor"`
	ADBPublicKey          string            `json:"adbPublicKey"`
	ADBHome               string            `json:"adbHome"`
	ADBKeyPath            string            `json:"adbKeyPath"`
	ADBPath               string            `json:"adbPath"`
	OneDeviceSerial       string            `json:"oneDeviceSerial"`
	Connection            *remoteConnection `json:"connection,omitempty"`
	OwnerPID              int               `json:"ownerPid"`
	OwnerBirth            string            `json:"ownerBirth"`
	ControlAddress        string            `json:"controlAddress"`
	ControlSecret         string            `json:"controlSecret"`
	ADBServerPID          int               `json:"adbServerPid,omitempty"`
	ADBServerBirth        string            `json:"adbServerBirth,omitempty"`
	ADBServerPort         int               `json:"adbServerPort"`
	DeviceEndpoint        string            `json:"deviceEndpoint"`
	LocalCleanupConfirmed bool              `json:"localCleanupConfirmed"`
}

type publicConnectionEnvironment struct {
	ConnectionID   string   `json:"connectionId"`
	LeaseID        string   `json:"leaseId"`
	DeviceID       string   `json:"deviceId"`
	Generation     int64    `json:"generation"`
	BootID         string   `json:"bootId"`
	Status         string   `json:"status"`
	ExpiresAt      string   `json:"expiresAt"`
	ADBBinary      string   `json:"adbBinary"`
	ADBHost        string   `json:"adbHost"`
	ADBServerPort  int      `json:"adbServerPort"`
	DeviceEndpoint string   `json:"deviceEndpoint"`
	ADBArgs        []string `json:"adbArgs"`
}

type connectionCommandOptions struct {
	taskDir string
	adbBin  string
}

func parseConnectionFlags(command string, args []string, connect bool) (connectionCommandOptions, *commandFailure) {
	var options connectionCommandOptions
	flags := newCommandFlagSet(command)
	flags.StringVar(&options.taskDir, "task-dir", "", "受保护的任务目录")
	if connect {
		flags.StringVar(&options.adbBin, "adb-bin", "", "平台工具锁定版本的原生 adb 绝对路径")
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || options.taskDir == "" || (connect && options.adbBin == "") {
		usage := "用法：droidharbor-cli " + command + " --task-dir <目录>"
		if connect {
			usage = "用法：droidharbor-cli connect --task-dir <目录> --adb-bin <原生 adb 绝对路径>"
		}
		return connectionCommandOptions{}, &commandFailure{Code: "INVALID_ARGUMENT", Message: usage, exitCode: 2}
	}
	return options, nil
}

func newCommandFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func lockedConnectionDir(path string) (string, func() error, *commandFailure) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, &commandFailure{Code: "TASK_STATE_ERROR", Message: err.Error(), exitCode: 2}
	}
	if err = secureTaskDirectory(absolute); err != nil {
		return "", nil, &commandFailure{Code: "TASK_STATE_PERMISSIONS", Message: err.Error(), exitCode: 2}
	}
	lock := flock.New(filepath.Join(absolute, ".connection.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		closeErr := lock.Close()
		return "", nil, &commandFailure{Code: "TASK_LOCK_ERROR", Message: errors.Join(err, closeErr).Error(), exitCode: 2}
	}
	if !locked {
		if err = lock.Close(); err != nil {
			return "", nil, &commandFailure{Code: "TASK_LOCK_ERROR", Message: err.Error(), exitCode: 2}
		}
		return "", nil, &commandFailure{Code: "TASK_BUSY", Message: "连接目录正在被另一个连接命令使用；未等待或重试", exitCode: 5}
	}
	taskStatePath := filepath.Join(absolute, taskStateName)
	if _, err = os.Stat(taskStatePath); err == nil {
		if err = protectPrivatePath(taskStatePath); err != nil {
			closeErr := lock.Close()
			return "", nil, &commandFailure{Code: "TASK_STATE_PERMISSIONS", Message: errors.Join(err, closeErr).Error(), exitCode: 2}
		}
	} else if !os.IsNotExist(err) {
		closeErr := lock.Close()
		return "", nil, &commandFailure{Code: "TASK_STATE_ERROR", Message: errors.Join(err, closeErr).Error(), exitCode: 2}
	}
	return absolute, lock.Close, nil
}

func loadConnectionState(directory string, task *taskState) (*connectionState, *commandFailure) {
	path := filepath.Join(directory, connectionStateName)
	if err := protectPrivatePath(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &commandFailure{Code: "CONNECTION_STATE_ERROR", Message: err.Error(), exitCode: 2}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, &commandFailure{Code: "CONNECTION_STATE_ERROR", Message: err.Error(), exitCode: 2}
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 65537))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > 65536 {
		return nil, &commandFailure{Code: "CONNECTION_STATE_ERROR", Message: errors.Join(readErr, closeErr, errors.New("连接状态读取不完整或文件超过 64 KiB")).Error(), exitCode: 2}
	}
	var state connectionState
	decodeErr := json.Unmarshal(data, &state)
	home, homeErr := filepath.Abs(state.ADBHome)
	keyPath, keyErr := filepath.Abs(state.ADBKeyPath)
	relativeHome, relErr := filepath.Rel(directory, home)
	validIdentity := state.OwnerPID == 0 && state.OwnerBirth == "" || state.OwnerPID > 0 && state.OwnerBirth != ""
	validServerIdentity := state.ADBServerPID == 0 && state.ADBServerBirth == "" || state.ADBServerPID > 0 && state.ADBServerBirth != ""
	validControl := state.ControlAddress == "" && state.ControlSecret == "" || state.ControlAddress != "" && validControlSecret(state.ControlSecret)
	if decodeErr != nil || state.TaskID != task.TaskID || state.LeaseID != task.LeaseID || !validHexValue(state.IdempotencyKey, 64) || state.ProtocolMajor != 1 || state.ADBPublicKey == "" || state.ADBHome == "" || state.ADBKeyPath == "" || state.OneDeviceSerial == "" || !strings.HasPrefix(state.OneDeviceSerial, "DroidHarbor-Task-") || !filepath.IsAbs(state.ADBHome) || !filepath.IsAbs(state.ADBKeyPath) || homeErr != nil || keyErr != nil || relErr != nil || relativeHome == "." || relativeHome == ".." || strings.HasPrefix(relativeHome, ".."+string(filepath.Separator)) || filepath.Clean(keyPath) != filepath.Join(filepath.Clean(home), ".android", "adbkey") || !validIdentity || !validServerIdentity || !validControl || state.ADBServerPort < 0 || state.ADBServerPort > 65535 {
		return nil, &commandFailure{Code: "CONNECTION_STATE_INVALID", Message: "本地连接状态无效或不匹配当前租约", exitCode: 2}
	}
	if state.ControlAddress != "" {
		if _, _, err := parseLoopbackControlAddress(state.ControlAddress); err != nil {
			return nil, &commandFailure{Code: "CONNECTION_STATE_INVALID", Message: "本地连接 control 地址无效", exitCode: 2}
		}
	}
	if state.Connection != nil && !validRemoteConnection(*state.Connection, task.LeaseID) {
		return nil, &commandFailure{Code: "CONNECTION_STATE_INVALID", Message: "本地连接响应字段无效", exitCode: 2}
	}
	return &state, nil
}

func validHexValue(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validControlSecret(value string) bool {
	return validHexValue(value, 64)
}

func saveConnectionState(directory string, state *connectionState) *commandFailure {
	data, err := json.Marshal(state)
	if err != nil {
		return &commandFailure{Code: "CONNECTION_STATE_ERROR", Message: err.Error(), exitCode: 2}
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".connection-state-*")
	if err != nil {
		return &commandFailure{Code: "CONNECTION_STATE_ERROR", Message: err.Error(), exitCode: 2}
	}
	name := temporary.Name()
	closed := false
	cleanup := func(primary error) *commandFailure {
		var closeErr error
		if !closed {
			closeErr = temporary.Close()
			closed = true
		}
		removeErr := os.Remove(name)
		if os.IsNotExist(removeErr) {
			removeErr = nil
		}
		return &commandFailure{Code: "CONNECTION_STATE_ERROR", Message: errors.Join(primary, closeErr, removeErr).Error(), exitCode: 2}
	}
	if err = temporary.Chmod(0600); err != nil {
		return cleanup(err)
	}
	if _, err = temporary.Write(data); err != nil {
		return cleanup(err)
	}
	if err = temporary.Sync(); err != nil {
		return cleanup(err)
	}
	if err = temporary.Close(); err != nil {
		closed = true
		return cleanup(err)
	}
	closed = true
	if err = os.Rename(name, filepath.Join(directory, connectionStateName)); err != nil {
		return cleanup(err)
	}
	if err = protectPrivatePath(filepath.Join(directory, connectionStateName)); err != nil {
		return &commandFailure{Code: "CONNECTION_STATE_ERROR", Message: err.Error(), exitCode: 2}
	}
	if err = syncDirectory(directory); err != nil {
		return &commandFailure{Code: "CONNECTION_STATE_ERROR", Message: err.Error(), exitCode: 2}
	}
	return nil
}

func syncDirectory(directory string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(syncErr, closeErr)
}

func publicEnvironment(state *connectionState, connection remoteConnection) publicConnectionEnvironment {
	endpoint := state.DeviceEndpoint
	return publicConnectionEnvironment{
		ConnectionID: connection.ConnectionID, LeaseID: connection.LeaseID, DeviceID: connection.DeviceID,
		Generation: connection.Generation, BootID: connection.BootID, Status: "active", ExpiresAt: connection.ExpiresAt,
		ADBBinary: state.ADBPath, ADBHost: "127.0.0.1", ADBServerPort: state.ADBServerPort, DeviceEndpoint: endpoint,
		ADBArgs: []string{"-H", "127.0.0.1", "-P", fmt.Sprint(state.ADBServerPort), "-s", endpoint},
	}
}

func connectionResourcePath(leaseID, connectionID string) string {
	return "/leases/" + url.PathEscape(leaseID) + "/connections/" + url.PathEscape(connectionID)
}

func validRemoteConnection(connection remoteConnection, leaseID string) bool {
	if connection.ConnectionID == "" || connection.LeaseID != leaseID || connection.DeviceID == "" || connection.Generation <= 0 || connection.BootID == "" || connection.ProtocolMajor != 1 || (connection.Status != "pending" && connection.Status != "active" && connection.Status != "closing" && connection.Status != "closed" && connection.Status != "failed") || connection.Features == nil {
		return false
	}
	parsed, err := time.Parse(time.RFC3339, connection.ExpiresAt)
	if err != nil {
		return false
	}
	_, offset := parsed.Zone()
	if offset != 0 {
		return false
	}
	expected := "/api/v1" + connectionResourcePath(leaseID, connection.ConnectionID) + "/tunnel"
	return connection.TunnelPath == expected
}

func readRemoteConnection(data []byte, leaseID string) (remoteConnection, error) {
	var connection remoteConnection
	if json.Unmarshal(data, &connection) != nil || !validRemoteConnection(connection, leaseID) {
		return remoteConnection{}, errors.New("平台未返回完整且有效的连接对象")
	}
	return connection, nil
}

func requestRemoteConnection(api *apiClient, task *taskState, method, resource string, body []byte, idempotencyKey string) (remoteConnection, int, string, *commandFailure) {
	headers := map[string]string{"Authorization": "Bearer " + task.TaskSecret}
	if idempotencyKey != "" {
		headers["Idempotency-Key"] = idempotencyKey
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	data, status, requestID, err := api.do(method, resource, reader, headers)
	if err != nil {
		code, exitCode, message := "CONNECTION_FAILED", 4, err.Error()
		if method == http.MethodPost || method == http.MethodDelete {
			code, exitCode = "RESULT_UNKNOWN", 6
			message = "连接写操作结果未知；保留原幂等身份，请显式查询或重试，不会自动重发"
		}
		return remoteConnection{}, status, requestID, &commandFailure{Code: code, Message: message, RequestID: requestID, exitCode: exitCode}
	}
	if status < 200 || status >= 300 {
		return remoteConnection{}, status, requestID, httpFailure(data, status, requestID)
	}
	if method == http.MethodPost && status != http.StatusCreated && status != http.StatusOK {
		return remoteConnection{}, status, requestID, &commandFailure{Code: "RESULT_UNKNOWN", Message: "平台返回了未声明的连接创建状态；结果未知，不会自动重发", RequestID: requestID, exitCode: 6}
	}
	if method == http.MethodDelete && status != http.StatusOK {
		return remoteConnection{}, status, requestID, &commandFailure{Code: "RESULT_UNKNOWN", Message: "平台返回了未声明的连接关闭状态；结果未知，不会自动重发", RequestID: requestID, exitCode: 6}
	}
	if method == http.MethodGet && status != http.StatusOK {
		return remoteConnection{}, status, requestID, &commandFailure{Code: "INVALID_RESPONSE", Message: "平台返回了未声明的连接响应状态", RequestID: requestID, exitCode: 4}
	}
	connection, decodeErr := readRemoteConnection(data, task.LeaseID)
	if decodeErr != nil {
		if method == http.MethodPost || method == http.MethodDelete {
			return remoteConnection{}, status, requestID, &commandFailure{Code: "RESULT_UNKNOWN", Message: decodeErr.Error() + "；写操作结果未知，不会自动重发", RequestID: requestID, exitCode: 6}
		}
		return remoteConnection{}, status, requestID, &commandFailure{Code: "INVALID_RESPONSE", Message: decodeErr.Error(), RequestID: requestID, exitCode: 4}
	}
	return connection, status, requestID, nil
}

func ensureAbsoluteADBPath(value string) (string, *commandFailure) {
	if !filepath.IsAbs(value) {
		return "", &commandFailure{Code: "INVALID_ARGUMENT", Message: "--adb-bin 必须是原生 adb 的绝对路径", exitCode: 2}
	}
	absolute := filepath.Clean(value)
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("adb 路径不是普通文件")
		}
		return "", &commandFailure{Code: "ADB_BINARY_ERROR", Message: err.Error(), exitCode: 2}
	}
	return absolute, nil
}

func connectionAttemptBody(state *connectionState) ([]byte, error) {
	return json.Marshal(struct {
		ProtocolMajor int    `json:"protocolMajor"`
		ADBPublicKey  string `json:"adbPublicKey"`
	}{ProtocolMajor: state.ProtocolMajor, ADBPublicKey: state.ADBPublicKey})
}
