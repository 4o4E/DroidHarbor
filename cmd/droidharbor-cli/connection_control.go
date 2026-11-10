package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"droidharbor/internal/diagnostics"
)

type localControlResponse struct {
	ConnectionID string                       `json:"connectionId"`
	Status       string                       `json:"status"`
	Environment  *publicConnectionEnvironment `json:"environment,omitempty"`
}

type connectionRuntime struct {
	directory    string
	task         *taskState
	state        *connectionState
	listener     net.Listener
	server       *http.Server
	adbServer    *ownedADBServer
	adbEnv       []string
	connectionMu sync.RWMutex
	connection   *remoteConnection
	mu           sync.Mutex
	ready        bool
	stopping     bool
	stopOnce     sync.Once
	stopCh       chan struct{}
	failureCh    chan error
	serveDone    chan struct{}
	handlers     sync.WaitGroup
}

func startLocalControl(directory string, task *taskState, state *connectionState, adbEnv []string) (*connectionRuntime, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	runtime := &connectionRuntime{
		directory: directory, task: task, state: state, listener: listener, adbEnv: adbEnv,
		stopCh: make(chan struct{}), failureCh: make(chan error, 1), serveDone: make(chan struct{}),
	}
	if state.Connection != nil {
		connection := *state.Connection
		runtime.connection = &connection
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/connections/", runtime.handleLocalControl)
	runtime.server = &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 10 * time.Second}
	address := "http://" + listener.Addr().String()
	state.ControlAddress = address
	go func() {
		defer close(runtime.serveDone)
		if serveErr := runtime.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			runtime.failureCh <- serveErr
		}
	}()
	return runtime, nil
}

func (r *connectionRuntime) setConnection(connection remoteConnection) {
	r.connectionMu.Lock()
	copy := connection
	r.connection = &copy
	r.connectionMu.Unlock()
}

func (r *connectionRuntime) connectionSnapshot() (remoteConnection, bool) {
	r.connectionMu.RLock()
	defer r.connectionMu.RUnlock()
	if r.connection == nil {
		return remoteConnection{}, false
	}
	return *r.connection, true
}

func (r *connectionRuntime) setADBServer(server *ownedADBServer) {
	r.mu.Lock()
	r.adbServer = server
	r.mu.Unlock()
}

func (r *connectionRuntime) adbServerAlive() bool {
	r.mu.Lock()
	server := r.adbServer
	r.mu.Unlock()
	if server == nil {
		return false
	}
	select {
	case <-server.done:
		return false
	default:
	}
	live, err := processIdentityMatches(int(server.pid), server.born)
	return err == nil && live
}

func (r *connectionRuntime) setReady(ready bool) {
	r.mu.Lock()
	r.ready = ready && !r.stopping
	r.mu.Unlock()
}

func (r *connectionRuntime) requestStop() {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.ready = false
		r.stopping = true
		r.mu.Unlock()
		close(r.stopCh)
	})
}

func (r *connectionRuntime) isReady() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready && !r.stopping
}

func (r *connectionRuntime) handleLocalControl(w http.ResponseWriter, request *http.Request) {
	r.handlers.Add(1)
	defer r.handlers.Done()
	remoteAddress, _, splitErr := net.SplitHostPort(request.RemoteAddr)
	remoteIP, parseErr := netip.ParseAddr(remoteAddress)
	if splitErr != nil || parseErr != nil || !remoteIP.IsLoopback() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	connection, ok := r.connectionSnapshot()
	if !ok {
		http.NotFound(w, request)
		return
	}
	connectionID := connection.ConnectionID
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] != "connections" || parts[2] != connectionID || request.Header.Get("X-DroidHarbor-Connection-ID") != connectionID {
		http.NotFound(w, request)
		return
	}
	authorization := request.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	token := strings.TrimPrefix(authorization, "Bearer ")
	if !validControlSecret(r.state.ControlSecret) || len(token) != len(r.state.ControlSecret) || subtle.ConstantTimeCompare([]byte(token), []byte(r.state.ControlSecret)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch request.Method {
	case http.MethodGet:
		r.handleStatus(w, request, connection)
	case http.MethodPost:
		r.handleStop(w, request, connection)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (r *connectionRuntime) handleStatus(w http.ResponseWriter, request *http.Request, connection remoteConnection) {
	if !r.isReady() || !r.adbServerAlive() {
		http.Error(w, "connection is not active", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	output, err := runNativeADB(ctx, r.state.ADBPath, r.adbEnv, r.state.ADBServerPort, []string{"-s", r.state.DeviceEndpoint, "get-state"})
	if err != nil {
		http.Error(w, "native adb get-state failed: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if strings.TrimSpace(string(output)) != "device" {
		http.Error(w, fmt.Sprintf("native adb get-state returned %q", strings.TrimSpace(string(output))), http.StatusServiceUnavailable)
		return
	}
	if !r.isReady() || !r.adbServerAlive() {
		http.Error(w, "native adb no longer reports the selected device online", http.StatusServiceUnavailable)
		return
	}
	environment := publicEnvironment(r.state, connection)
	writeControlJSON(w, localControlResponse{ConnectionID: connection.ConnectionID, Status: "active", Environment: &environment})
}
func (r *connectionRuntime) handleStop(w http.ResponseWriter, request *http.Request, connection remoteConnection) {
	r.requestStop()
	// Acknowledge the request, not completed cleanup. The CLI waits for the
	// identified owner and its Server to exit before reporting public closed.
	writeControlJSON(w, localControlResponse{ConnectionID: connection.ConnectionID, Status: "stopping"})
}

func writeControlJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn("local_control_response_write_failed", "cause", diagnostics.Cause(err))
	}
}

func stopLocalControl(runtime *connectionRuntime) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := runtime.server.Shutdown(ctx)
	if shutdownErr != nil {
		closeErr := runtime.server.Close()
		<-runtime.serveDone
		runtime.handlers.Wait()
		return errors.Join(shutdownErr, closeErr)
	}
	<-runtime.serveDone
	runtime.handlers.Wait()
	return nil
}

func parseLoopbackControlAddress(raw string) (string, int, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "http" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", 0, errors.New("本地 control 地址无效")
	}
	host, portValue, err := net.SplitHostPort(endpoint.Host)
	if err != nil {
		return "", 0, errors.New("本地 control 地址无效")
	}
	address, err := netip.ParseAddr(host)
	if err != nil || !address.IsLoopback() {
		return "", 0, errors.New("本地 control 地址不是 loopback")
	}
	port, err := strconv.Atoi(portValue)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, errors.New("本地 control 端口无效")
	}
	return strings.TrimRight(endpoint.String(), "/"), port, nil
}

func savedControlClient(state *connectionState) (*http.Client, string, error) {
	endpoint, _, err := parseLoopbackControlAddress(state.ControlAddress)
	if err != nil || !validControlSecret(state.ControlSecret) || state.Connection == nil {
		return nil, "", errors.New("本地 control 身份字段无效")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext}
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return client, endpoint, nil
}

func callLocalControl(state *connectionState, command string) (localControlResponse, error) {
	live, err := processIdentityMatches(state.OwnerPID, state.OwnerBirth)
	if err != nil {
		return localControlResponse{}, fmt.Errorf("连接进程身份无法核验：%w", err)
	}
	if !live {
		return localControlResponse{}, errors.New("原连接进程已退出或 PID 已复用")
	}
	client, endpoint, err := savedControlClient(state)
	if err != nil {
		return localControlResponse{}, err
	}
	defer client.CloseIdleConnections()
	resource := endpoint + "/v1/connections/" + url.PathEscape(state.Connection.ConnectionID)
	method := http.MethodGet
	if command == "stop" {
		method = http.MethodPost
	}
	request, err := http.NewRequest(method, resource, nil)
	if err != nil {
		return localControlResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+state.ControlSecret)
	request.Header.Set("X-DroidHarbor-Connection-ID", state.Connection.ConnectionID)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return localControlResponse{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || len(data) > 65536 {
		return localControlResponse{}, errors.Join(readErr, closeErr, errors.New("本地 control 响应不完整或超过 64 KiB"))
	}
	if response.StatusCode != http.StatusOK {
		return localControlResponse{}, fmt.Errorf("本地 control 返回 HTTP %d：%s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	var result localControlResponse
	if json.Unmarshal(data, &result) != nil || result.ConnectionID != state.Connection.ConnectionID {
		return localControlResponse{}, errors.New("本地 control 响应身份无效")
	}
	return result, nil
}

func runNativeADB(ctx context.Context, binary string, environment []string, serverPort int, args []string) ([]byte, error) {
	fullArgs := []string{"-H", "127.0.0.1", "-P", strconv.Itoa(serverPort)}
	fullArgs = append(fullArgs, args...)
	command := exec.CommandContext(ctx, binary, fullArgs...)
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("adb %s 失败：%w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}
