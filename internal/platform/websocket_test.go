package platform

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWebsocketCleanupAfterPeerTCPReset(t *testing.T) {
	rawServer := make(chan net.Conn, 1)
	closeResult := make(chan error, 1)
	peerReset := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrade := &websocketUpgradeResponse{ResponseWriter: w, server: &Server{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, request: r}
		ws, err := websocket.Accept(upgrade, r, nil)
		if err != nil {
			closeResult <- err
			return
		}
		stream := &websocketStream{Conn: websocket.NetConn(r.Context(), ws, websocket.MessageBinary), ws: ws, socket: upgrade.socket}
		<-peerReset
		closeResult <- stream.Close()
	}))
	server.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		rawServer <- conn.(*tls.Conn).NetConn()
		return ctx
	}
	server.StartTLS()
	defer server.Close()
	defer func() {
		select {
		case <-peerReset:
		default:
			close(peerReset)
		}
	}()
	client := server.Client()
	transport := client.Transport.(*http.Transport).Clone()
	client.Transport = transport
	defer transport.CloseIdleConnections()
	rawClient := make(chan *net.TCPConn, 1)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			rawClient <- conn.(*net.TCPConn)
		}
		return conn, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(server.URL, "https"), &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	peer := <-rawClient
	if err := peer.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	raw := <-rawServer
	if err := raw.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var probe [1]byte
	_, resetErr := raw.Read(probe[:])
	var networkErr net.Error
	if resetErr == nil || errors.Is(resetErr, io.EOF) || errors.As(resetErr, &networkErr) && networkErr.Timeout() {
		t.Fatal("peer reset was not observed", resetErr)
	}
	close(peerReset)
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("owned socket cleanup failed after peer reset: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("cleanup did not terminate")
	}
	if _, err := raw.Read(probe[:]); !errors.Is(err, net.ErrClosed) {
		t.Fatal("owned socket was not closed", err)
	}
}
