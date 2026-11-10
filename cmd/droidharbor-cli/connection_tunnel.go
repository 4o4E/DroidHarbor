package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const tunnelBufferSize = 32 * 1024

type connectionTunnel struct {
	ws       *websocket.Conn
	socket   net.Conn
	once     sync.Once
	closeErr error
}

func (t *connectionTunnel) CloseNow() error {
	t.once.Do(func() {
		// Confirm physical socket closure, then join WebSocket workers.
		socketErr := t.socket.Close()
		if errors.Is(socketErr, net.ErrClosed) {
			socketErr = nil
		}
		websocketErr := t.ws.CloseNow()
		if errors.Is(websocketErr, net.ErrClosed) {
			websocketErr = nil
		}
		t.closeErr = errors.Join(socketErr, websocketErr)
	})
	return t.closeErr
}

func dialConnectionTunnel(ctx context.Context, api *apiClient, connection remoteConnection, taskSecret string) (*connectionTunnel, *commandFailure) {
	endpoint := *api.base
	endpoint.Scheme = "ws"
	if api.base.Scheme == "https" {
		endpoint.Scheme = "wss"
	}
	path, err := url.Parse(connection.TunnelPath)
	if err != nil || path.IsAbs() || path.RawQuery != "" || path.Fragment != "" || !strings.HasPrefix(path.Path, "/api/v1/leases/") {
		return nil, &commandFailure{Code: "INVALID_RESPONSE", Message: "平台返回了无效的 tunnelPath", exitCode: 4}
	}
	endpoint.Path = path.Path
	endpoint.RawPath = path.RawPath
	transport, ok := api.client.Transport.(*http.Transport)
	if !ok {
		return nil, &commandFailure{Code: "CONNECTION_CONFIGURATION_ERROR", Message: "HTTP transport 不支持 WebSocket", exitCode: 2}
	}
	client := &http.Client{
		Transport:     transport.Clone(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var socket net.Conn
	dialCtx = httptrace.WithClientTrace(dialCtx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		socket = info.Conn
		if underlying, ok := socket.(interface{ NetConn() net.Conn }); ok {
			socket = underlying.NetConn()
		}
	}})
	conn, response, err := websocket.Dial(dialCtx, endpoint.String(), &websocket.DialOptions{
		HTTPClient:      client,
		HTTPHeader:      http.Header{"Authorization": []string{"Bearer " + taskSecret}},
		Subprotocols:    []string{"droidharbor.adb.v1"},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if response != nil {
			requestID := response.Header.Get("X-Request-ID")
			if response.Body == nil {
				return nil, httpFailure(nil, response.StatusCode, requestID)
			}
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
			closeErr := response.Body.Close()
			if readErr == nil && closeErr == nil && len(body) <= 65536 {
				return nil, httpFailure(body, response.StatusCode, requestID)
			}
			return nil, &commandFailure{Code: "WEBSOCKET_HANDSHAKE_FAILED", Message: errors.Join(err, readErr, closeErr, errors.New("WebSocket 握手响应不完整或超过 64 KiB")).Error(), RequestID: requestID, exitCode: 4}
		}
		return nil, &commandFailure{Code: "WEBSOCKET_HANDSHAKE_FAILED", Message: err.Error(), exitCode: 4}
	}
	tunnel := &connectionTunnel{ws: conn, socket: socket}
	if conn.Subprotocol() != "droidharbor.adb.v1" {
		_ = tunnel.CloseNow()
		return nil, &commandFailure{Code: "INVALID_RESPONSE", Message: "WebSocket 服务端未协商 droidharbor.adb.v1", exitCode: 4}
	}
	return tunnel, nil
}

func bridgeConnection(ctx context.Context, local net.Conn, tunnel *connectionTunnel) error {
	bridgeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	tunnelStream := websocket.NetConn(bridgeCtx, tunnel.ws, websocket.MessageBinary)
	results := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		results <- copyConnectionStream(tunnelStream, local)
	}()
	go func() {
		defer workers.Done()
		results <- copyConnectionStream(local, tunnelStream)
	}()

	var primary error
	select {
	case primary = <-results:
		if primary == nil && ctx.Err() == nil {
			primary = errors.New("ADB tunnel closed")
		}
	case <-ctx.Done():
		primary = ctx.Err()
	}
	cancel()
	closeNowErr := tunnel.CloseNow()
	adapterCloseErr := tunnelStream.Close()
	localCloseErr := local.Close()
	workers.Wait()
	if errors.Is(localCloseErr, net.ErrClosed) {
		localCloseErr = nil
	}
	if errors.Is(primary, context.Canceled) && ctx.Err() != nil {
		primary = nil
	}
	if errors.Is(closeNowErr, net.ErrClosed) {
		closeNowErr = nil
	}
	if errors.Is(adapterCloseErr, net.ErrClosed) {
		adapterCloseErr = nil
	}
	return errors.Join(primary, closeNowErr, adapterCloseErr, localCloseErr)
}

func copyConnectionStream(destination io.Writer, source io.Reader) error {
	// Keep TCP's ReaderFrom/WriterTo optimizations from bypassing this buffer.
	_, err := io.CopyBuffer(struct{ io.Writer }{destination}, struct{ io.Reader }{source}, make([]byte, tunnelBufferSize))
	return err
}
