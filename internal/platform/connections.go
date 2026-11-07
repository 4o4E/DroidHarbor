package platform

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"droidharbor/internal/connections"
	"droidharbor/internal/leases"
)

func (s *Server) connectionOwner(w http.ResponseWriter, r *http.Request, write bool) (leases.Principal, bool) {
	if s.Connections == nil || s.Leases == nil {
		s.fail(w, r, 503, "FEATURE_UNAVAILABLE", "ADB 数据通道未启用")
		return leases.Principal{}, false
	}
	principal, ok := s.principal(w, r, write)
	if !ok {
		return leases.Principal{}, false
	}
	if principal.Kind != leases.PrincipalTask {
		s.fail(w, r, 403, "FORBIDDEN", "连接只能由对应任务凭证访问")
		return leases.Principal{}, false
	}
	return principal, true
}

func (s *Server) createConnection(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.connectionOwner(w, r, true)
	if !ok {
		return
	}
	var request connections.Request
	if !s.decode(w, r, &request, false) {
		return
	}
	view, created, err := s.Connections.Create(r.Context(), s.Leases, principal, r.PathValue("id"), r.Header.Get("Idempotency-Key"), request)
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.json(w, status, view)
}

func (s *Server) connectionLease(w http.ResponseWriter, r *http.Request, write bool) (leases.Lease, bool) {
	principal, ok := s.connectionOwner(w, r, write)
	if !ok {
		return leases.Lease{}, false
	}
	lease, err := s.Leases.Get(r.Context(), principal, r.PathValue("id"))
	if err != nil {
		s.domainError(w, r, err)
		return leases.Lease{}, false
	}
	return lease, true
}

func (s *Server) getConnection(w http.ResponseWriter, r *http.Request) {
	lease, ok := s.connectionLease(w, r, false)
	if !ok {
		return
	}
	view, err := s.Connections.Get(r.Context(), lease, r.PathValue("connectionId"))
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, 200, view)
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	lease, ok := s.connectionLease(w, r, true)
	if !ok {
		return
	}
	view, err := s.Connections.Delete(r.Context(), lease, r.PathValue("connectionId"))
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, 200, view)
}

func (s *Server) connectionTunnel(w http.ResponseWriter, r *http.Request) {
	lease, ok := s.connectionLease(w, r, false)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		s.fail(w, r, 422, "INVALID_REQUEST", "通道地址不接受查询参数")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.Origin {
		s.fail(w, r, 403, "ORIGIN_DENIED", "请求来源不匹配")
		return
	}
	compatible := false
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, offered := range strings.Split(header, ",") {
			if strings.TrimSpace(offered) == connections.Subprotocol {
				compatible = true
			}
		}
	}
	if !compatible {
		s.domainError(w, r, connections.ErrProtocol)
		return
	}
	if err := s.Leases.Check(lease.ID, lease.Generation, lease.BootID); err != nil {
		s.domainError(w, r, err)
		return
	}
	tunnel, err := s.Connections.Begin(r.Context(), lease, r.PathValue("connectionId"))
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	defer func() {
		if err := tunnel.Close(); err != nil {
			s.Logger.Error("tunnel_cleanup_failed", "connectionId", r.PathValue("connectionId"))
		}
	}()
	upgrade := &websocketUpgradeResponse{ResponseWriter: w, server: s, request: r}
	// Origin was checked exactly above. Skip the library's additional wildcard
	// host matcher, which differs behind proxies and for literal IPv6 origins.
	ws, err := websocket.Accept(upgrade, r, &websocket.AcceptOptions{Subprotocols: []string{connections.Subprotocol}, InsecureSkipVerify: true})
	if err != nil {
		return
	} // Accept has already written the HTTP rejection.
	stream := &websocketStream{Conn: websocket.NetConn(tunnel.Context(), ws, websocket.MessageBinary), ws: ws, socket: upgrade.socket}
	if err = tunnel.Serve(stream); err != nil {
		// Peer/upstream error text can include device data; log identity, not payload.
		s.Logger.Info("adb_transport_ended", "connectionId", r.PathValue("connectionId"))
	}
}

// Preserve API error JSON without duplicating the WebSocket library's handshake
// validation. Unwrap retains its access to the underlying HTTP hijacker.
type websocketUpgradeResponse struct {
	http.ResponseWriter
	server   *Server
	request  *http.Request
	socket   net.Conn
	rejected bool
}

func (w *websocketUpgradeResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *websocketUpgradeResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.socket = conn
		if secure, ok := conn.(*tls.Conn); ok {
			w.socket = secure.NetConn()
		}
	}
	return conn, buffered, err
}
func (w *websocketUpgradeResponse) WriteHeader(status int) {
	if status >= 400 {
		w.rejected = true
		w.server.fail(w.ResponseWriter, w.request, status, "WEBSOCKET_UPGRADE_FAILED", "WebSocket 握手失败")
		return
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *websocketUpgradeResponse) Write(data []byte) (int, error) {
	if w.rejected {
		return len(data), nil
	}
	return w.ResponseWriter.Write(data)
}

type websocketStream struct {
	net.Conn
	ws       *websocket.Conn
	socket   net.Conn
	once     sync.Once
	closeErr error
}

func (c *websocketStream) Close() error {
	c.once.Do(func() {
		// Close the owned socket first. TLS close_notify can fail after a peer
		// reset even though TLS closed the socket; it is not resource leakage.
		socketErr := c.socket.Close()
		if errors.Is(socketErr, net.ErrClosed) {
			socketErr = nil
		}
		immediate := c.ws.CloseNow()
		adapter := c.Conn.Close() // Stop NetConn timers/contexts after immediate close.
		if errors.Is(immediate, net.ErrClosed) {
			immediate = nil
		}
		if errors.Is(adapter, net.ErrClosed) {
			adapter = nil
		}
		c.closeErr = errors.Join(socketErr, immediate, adapter)
	})
	return c.closeErr
}
