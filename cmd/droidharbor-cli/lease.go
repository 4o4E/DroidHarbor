package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
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
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gofrs/flock"
)

type taskState struct {
	Server         string   `json:"server"`
	TaskID         string   `json:"taskId"`
	TaskSecret     string   `json:"taskSecret"`
	IdempotencyKey string   `json:"idempotencyKey"`
	RequiredTags   []string `json:"requiredTags"`
	LeaseID        string   `json:"leaseId,omitempty"`
}

type leaseResponse struct {
	LeaseID         string  `json:"leaseId"`
	DeviceID        string  `json:"deviceId"`
	Generation      int64   `json:"generation"`
	BootID          string  `json:"bootId"`
	Status          string  `json:"status"`
	ClaimedAt       string  `json:"claimedAt"`
	ExpiresAt       string  `json:"expiresAt"`
	EndedAt         *string `json:"endedAt"`
	EndReason       string  `json:"endReason,omitempty"`
	AllocationState string  `json:"allocationState"`
}

type leaseCommandOptions struct {
	taskDir        string
	caFile         string
	server         string
	credentialFile string
	taskID         string
	tags           []string
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

const taskStateName = "task-state.json"

func randomHex(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func canonicalRequiredTags(tags []string) ([]string, error) {
	set := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || !utf8.ValidString(tag) {
			return nil, errors.New("标签不能为空且必须为有效 UTF-8")
		}
		set[tag] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for tag := range set {
		result = append(result, tag)
	}
	sort.Strings(result)
	return result, nil
}

func parseLeaseFlags(command string, args []string, claim bool) (leaseCommandOptions, *commandFailure) {
	var options leaseCommandOptions
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.taskDir, "task-dir", "", "受保护的任务目录")
	flags.StringVar(&options.caFile, "ca-file", "", "可选的额外 PEM CA 证书")
	var tags stringSliceFlag
	if claim {
		flags.StringVar(&options.server, "server", "", "平台 HTTPS origin")
		flags.StringVar(&options.credentialFile, "credential-file", "", "含 token 的 JSON 文件")
		flags.StringVar(&options.taskID, "task-id", "", "可选的外部任务 ID")
		flags.Var(&tags, "tag", "所需标签（可重复）")
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || options.taskDir == "" || (claim && (options.server == "" || options.credentialFile == "")) {
		usage := "用法：droidharbor-cli " + command + " --task-dir <目录> [--ca-file <PEM 文件>]"
		if claim {
			usage = "用法：droidharbor-cli claim --task-dir <目录> --server <https://host:port> --credential-file <JSON 文件> [--tag <标签>]... [--task-id <ID>] [--ca-file <PEM 文件>]"
		}
		return leaseCommandOptions{}, &commandFailure{Code: "INVALID_ARGUMENT", Message: usage, exitCode: 2}
	}
	options.tags = tags
	return options, nil
}

func lockedTaskDir(path string, create bool) (string, func() error, *commandFailure) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, &commandFailure{Code: "TASK_STATE_ERROR", Message: err.Error(), exitCode: 2}
	}
	if create {
		err = os.Mkdir(absolute, 0700)
		if err != nil && !os.IsExist(err) {
			return "", nil, &commandFailure{Code: "TASK_STATE_ERROR", Message: err.Error(), exitCode: 2}
		}
		if err == nil {
			if err = os.Chmod(absolute, 0700); err != nil {
				return "", nil, &commandFailure{Code: "TASK_STATE_ERROR", Message: err.Error(), exitCode: 2}
			}
		}
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", nil, &commandFailure{Code: "TASK_STATE_ERROR", Message: "任务目录不存在或不是目录", exitCode: 2}
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return "", nil, &commandFailure{Code: "TASK_STATE_PERMISSIONS", Message: "任务目录权限过宽；请设置为 0700", exitCode: 2}
	}

	lock := flock.New(filepath.Join(absolute, ".task.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		closeErr := lock.Close()
		return "", nil, &commandFailure{Code: "TASK_LOCK_ERROR", Message: errors.Join(err, closeErr).Error(), exitCode: 2}
	}
	if !locked {
		if err = lock.Close(); err != nil {
			return "", nil, &commandFailure{Code: "TASK_LOCK_ERROR", Message: err.Error(), exitCode: 2}
		}
		return "", nil, &commandFailure{Code: "TASK_BUSY", Message: "任务目录正在被另一个 CLI 命令使用；未等待或重试", exitCode: 5}
	}
	return absolute, lock.Close, nil
}

func loadTaskState(directory string) (*taskState, *commandFailure) {
	file, err := os.Open(filepath.Join(directory, taskStateName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &commandFailure{Code: "TASK_STATE_MISSING", Message: "任务状态不存在；请使用 claim 显式认领", exitCode: 2}
		}
		return nil, &commandFailure{Code: "TASK_STATE_ERROR", Message: err.Error(), exitCode: 2}
	}
	info, err := file.Stat()
	if err != nil {
		closeErr := file.Close()
		return nil, &commandFailure{Code: "TASK_STATE_ERROR", Message: errors.Join(err, closeErr).Error(), exitCode: 2}
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		closeErr := file.Close()
		message := "任务状态文件权限过宽；请设置为 0600"
		if closeErr != nil {
			message = errors.Join(errors.New(message), closeErr).Error()
		}
		return nil, &commandFailure{Code: "TASK_STATE_PERMISSIONS", Message: message, exitCode: 2}
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 65537))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > 65536 {
		return nil, &commandFailure{Code: "TASK_STATE_ERROR", Message: errors.Join(readErr, closeErr, errors.New("无法完整读取任务状态或文件超过 64 KiB")).Error(), exitCode: 2}
	}
	var state taskState
	if json.Unmarshal(data, &state) != nil || state.Server == "" || state.TaskID == "" || len(state.TaskSecret) != 64 || state.IdempotencyKey == "" {
		return nil, &commandFailure{Code: "TASK_STATE_INVALID", Message: "任务状态文件无效或不完整", exitCode: 2}
	}
	return &state, nil
}

func saveTaskState(directory string, state *taskState) *commandFailure {
	data, err := json.Marshal(state)
	if err != nil {
		return &commandFailure{Code: "TASK_STATE_ERROR", Message: err.Error(), exitCode: 2}
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".task-state-*")
	if err != nil {
		return &commandFailure{Code: "TASK_STATE_ERROR", Message: err.Error(), exitCode: 2}
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
		return &commandFailure{Code: "TASK_STATE_ERROR", Message: errors.Join(primary, closeErr, removeErr).Error(), exitCode: 2}
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
	if err = os.Rename(name, filepath.Join(directory, taskStateName)); err != nil {
		return cleanup(err)
	}
	if runtime.GOOS != "windows" {
		dir, openErr := os.Open(directory)
		if openErr != nil {
			return &commandFailure{Code: "TASK_STATE_ERROR", Message: openErr.Error(), exitCode: 2}
		}
		syncErr := dir.Sync()
		closeErr := dir.Close()
		if syncErr != nil || closeErr != nil {
			return &commandFailure{Code: "TASK_STATE_ERROR", Message: errors.Join(syncErr, closeErr).Error(), exitCode: 2}
		}
	}
	return nil
}

func claim(args []string) (json.RawMessage, *commandFailure) {
	return runLeaseCommand("claim", args)
}
func status(args []string) (json.RawMessage, *commandFailure) {
	return runLeaseCommand("status", args)
}
func renew(args []string) (json.RawMessage, *commandFailure) {
	return runLeaseCommand("renew", args)
}
func release(args []string) (json.RawMessage, *commandFailure) {
	return runLeaseCommand("release", args)
}

func runLeaseCommand(command string, args []string) (result json.RawMessage, failure *commandFailure) {
	isClaim := command == "claim"
	options, failure := parseLeaseFlags(command, args, isClaim)
	if failure != nil {
		return nil, failure
	}
	directory, unlock, failure := lockedTaskDir(options.taskDir, isClaim)
	if failure != nil {
		return nil, failure
	}
	defer func() {
		if err := unlock(); err != nil {
			result = nil
			if failure != nil {
				failure.Message = errors.Join(errors.New(failure.Message), err).Error()
				return
			}
			failure = &commandFailure{Code: "TASK_LOCK_ERROR", Message: "平台响应已收到，但任务目录锁关闭失败；请先查询状态：" + err.Error(), exitCode: 2}
		}
	}()

	state, callerToken, failure := prepareTaskState(directory, options, isClaim)
	if failure != nil {
		return nil, failure
	}
	api, failure := newAPI(state.Server, options.caFile)
	if failure != nil {
		return nil, failure
	}
	defer api.client.CloseIdleConnections()

	bearer := state.TaskSecret
	if isClaim {
		bearer = callerToken
	}
	capabilities, statusCode, requestID, err := api.do("GET", "/capabilities", nil, map[string]string{"Authorization": "Bearer " + bearer})
	if err != nil {
		return nil, &commandFailure{Code: "CONNECTION_FAILED", Message: err.Error(), RequestID: requestID, exitCode: 4}
	}
	if statusCode != http.StatusOK {
		return nil, httpFailure(capabilities, statusCode, requestID)
	}
	var apiCapabilities struct {
		APIMajor int             `json:"apiMajor"`
		Features map[string]bool `json:"features"`
	}
	if json.Unmarshal(capabilities, &apiCapabilities) != nil || apiCapabilities.Features == nil {
		return nil, &commandFailure{Code: "INVALID_RESPONSE", Message: "平台未返回有效的能力对象", RequestID: requestID, exitCode: 4}
	}
	if apiCapabilities.APIMajor != 1 {
		return nil, &commandFailure{Code: "API_INCOMPATIBLE", Message: fmt.Sprintf("CLI 要求 API major 1，平台返回 %d", apiCapabilities.APIMajor), RequestID: requestID, exitCode: 5}
	}
	if isClaim && !apiCapabilities.Features["leaseClaim"] {
		return nil, &commandFailure{Code: "CAPABILITY_UNAVAILABLE", Message: "平台未声明 leaseClaim 能力；本次未提交认领", RequestID: requestID, exitCode: 5}
	}

	method, path, body := leaseRequest(command, state)
	headers := map[string]string{"Authorization": "Bearer " + state.TaskSecret}
	if isClaim {
		headers["Authorization"] = "Bearer " + callerToken
		headers["Idempotency-Key"] = state.IdempotencyKey
	}
	data, responseStatus, requestID, err := api.do(method, path, body, headers)
	if err != nil {
		if isWriteCommand(command) {
			return nil, &commandFailure{Code: "RESULT_UNKNOWN", Message: "写操作结果未知；不会自动重发，请先显式查询状态或按原幂等策略处理", RequestID: requestID, exitCode: 6}
		}
		return nil, &commandFailure{Code: "CONNECTION_FAILED", Message: err.Error(), RequestID: requestID, exitCode: 4}
	}
	if responseStatus < 200 || responseStatus >= 300 {
		return nil, httpFailure(data, responseStatus, requestID)
	}
	if !validLeaseSuccessStatus(command, responseStatus) {
		return nil, invalidLeaseResponse(command, requestID, "平台返回了租约 API 未声明的成功状态")
	}
	var lease leaseResponse
	if json.Unmarshal(data, &lease) != nil || !validLeaseResponse(lease) || (!isClaim && lease.LeaseID != state.LeaseID) {
		return nil, invalidLeaseResponse(command, requestID, "平台租约响应无效或缺少必要身份/状态字段")
	}
	if isClaim {
		state.LeaseID = lease.LeaseID
		if failure = saveTaskState(directory, state); failure != nil {
			return nil, &commandFailure{Code: "RESULT_UNKNOWN", Message: "服务端已返回 leaseId，但本地状态持久化失败；保留目录并修复权限后用相同凭证显式 claim 恢复", RequestID: requestID, exitCode: 6}
		}
	}
	encoded, err := json.Marshal(lease)
	if err != nil {
		return nil, &commandFailure{Code: "INVALID_RESPONSE", Message: "无法编码租约响应", RequestID: requestID, exitCode: 4}
	}
	return encoded, nil
}

func prepareTaskState(directory string, options leaseCommandOptions, claim bool) (*taskState, string, *commandFailure) {
	if !claim {
		state, failure := loadTaskState(directory)
		if failure != nil {
			return nil, "", failure
		}
		if state.LeaseID == "" {
			return nil, "", &commandFailure{Code: "RESULT_UNKNOWN", Message: "尚未收到 leaseId，首次认领结果未知；使用相同凭证显式运行 claim 恢复原请求，不能新建任务", exitCode: 6}
		}
		return state, "", nil
	}

	base, failure := parseHTTPSOrigin(options.server)
	if failure != nil {
		return nil, "", failure
	}
	server := strings.TrimRight(base.String(), "/")
	statePath := filepath.Join(directory, taskStateName)
	_, err := os.Stat(statePath)
	if err == nil {
		state, stateFailure := loadTaskState(directory)
		if stateFailure != nil {
			return nil, "", stateFailure
		}
		if state.Server != server || (options.taskID != "" && state.TaskID != options.taskID) {
			return nil, "", &commandFailure{Code: "TASK_DIRECTORY_CONFLICT", Message: "任务目录已绑定其他平台或 taskId", exitCode: 2}
		}
		tags, tagErr := canonicalRequiredTags(options.tags)
		if tagErr != nil {
			return nil, "", &commandFailure{Code: "INVALID_ARGUMENT", Message: tagErr.Error(), exitCode: 2}
		}
		if !sameStrings(state.RequiredTags, tags) {
			return nil, "", &commandFailure{Code: "TASK_DIRECTORY_CONFLICT", Message: "已保存的认领请求参数不匹配；禁止更改任务秘密或幂等键", exitCode: 2}
		}
		token, tokenFailure := readCredential(options.credentialFile)
		return state, token, tokenFailure
	}
	if !os.IsNotExist(err) {
		return nil, "", &commandFailure{Code: "TASK_STATE_ERROR", Message: err.Error(), exitCode: 2}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, "", &commandFailure{Code: "TASK_STATE_ERROR", Message: err.Error(), exitCode: 2}
	}
	if len(entries) > 1 || (len(entries) == 1 && entries[0].Name() != ".task.lock") {
		return nil, "", &commandFailure{Code: "TASK_DIRECTORY_CONFLICT", Message: "任务目录非空且没有有效任务状态", exitCode: 2}
	}

	taskID := options.taskID
	if taskID == "" {
		taskID, err = randomHex(16)
		if err != nil {
			return nil, "", &commandFailure{Code: "RANDOM_FAILED", Message: "无法生成任务 ID", exitCode: 2}
		}
	} else if strings.TrimSpace(taskID) == "" || !utf8.ValidString(taskID) {
		return nil, "", &commandFailure{Code: "INVALID_ARGUMENT", Message: "taskId 不能为空且必须为有效 UTF-8", exitCode: 2}
	}
	secret, err := randomHex(32)
	if err != nil {
		return nil, "", &commandFailure{Code: "RANDOM_FAILED", Message: "无法生成任务秘密", exitCode: 2}
	}
	idempotencyKey, err := randomHex(32)
	if err != nil {
		return nil, "", &commandFailure{Code: "RANDOM_FAILED", Message: "无法生成幂等键", exitCode: 2}
	}
	tags, err := canonicalRequiredTags(options.tags)
	if err != nil {
		return nil, "", &commandFailure{Code: "INVALID_ARGUMENT", Message: err.Error(), exitCode: 2}
	}
	state := &taskState{Server: server, TaskID: taskID, TaskSecret: secret, IdempotencyKey: idempotencyKey, RequiredTags: tags}
	if failure := saveTaskState(directory, state); failure != nil {
		return nil, "", failure
	}
	token, failure := readCredential(options.credentialFile)
	return state, token, failure
}

func leaseRequest(command string, state *taskState) (string, string, io.Reader) {
	if command == "claim" {
		body, _ := json.Marshal(struct {
			TaskID       string   `json:"taskId"`
			TaskSecret   string   `json:"taskSecret"`
			RequiredTags []string `json:"requiredTags"`
		}{TaskID: state.TaskID, TaskSecret: state.TaskSecret, RequiredTags: state.RequiredTags})
		return http.MethodPost, "/leases", bytes.NewReader(body)
	}
	path := "/leases/" + url.PathEscape(state.LeaseID)
	switch command {
	case "status":
		return http.MethodGet, path, nil
	case "renew":
		return http.MethodPost, path + "/renew", strings.NewReader("{}")
	case "release":
		return http.MethodDelete, path, nil
	default:
		return "", "", nil
	}
}

func validLeaseSuccessStatus(command string, status int) bool {
	if command == "release" {
		return status == http.StatusOK || status == http.StatusAccepted
	}
	return status == http.StatusOK
}

func validLeaseResponse(lease leaseResponse) bool {
	return lease.LeaseID != "" && lease.DeviceID != "" && lease.BootID != "" &&
		(lease.Status == "active" || lease.Status == "ended") && lease.Generation > 0 &&
		lease.ClaimedAt != "" && lease.ExpiresAt != "" && lease.AllocationState != ""
}

func invalidLeaseResponse(command, requestID, message string) *commandFailure {
	if isWriteCommand(command) {
		return &commandFailure{Code: "RESULT_UNKNOWN", Message: message + "；写操作结果未知，不会自动重发", RequestID: requestID, exitCode: 6}
	}
	return &commandFailure{Code: "INVALID_RESPONSE", Message: message, RequestID: requestID, exitCode: 4}
}

func isWriteCommand(command string) bool {
	return command == "claim" || command == "renew" || command == "release"
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
