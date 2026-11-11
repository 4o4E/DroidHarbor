// Package media coordinates shared device capture and independently authorized browser sessions.
package media

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"sync"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/capture"
	"droidharbor/internal/events"
	"droidharbor/internal/leases"
	"droidharbor/internal/rtc"
	"github.com/pion/rtp"
)

const (
	maxSessions           = 64
	maxQueuedFrames       = 8
	maxFrameQueueBytes    = 4 << 20
	maxControlQueueBytes  = 2 << 20
	controlTimeout        = 5 * time.Second
	negotiationTimeout    = 20 * time.Second
	peerConnectionTimeout = 30 * time.Second
	keyFrameTimeout       = 5 * time.Second
	cleanupTimeout        = 10 * time.Second
	clipboardTimeout      = 10 * time.Second
	maxScreenshotBytes    = 32 << 20
	maxFrameBoundaries    = 256
	screenshotTimeout     = 30 * time.Second
	initialFrameMaxAge    = 250 * time.Millisecond
)

type Error struct {
	Code, Message string
	Status        int
	Cause         error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Cause }
func problem(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

type Source interface {
	capture.Source
	LinkContext(leases.Link) (context.Context, error)
}
type Dependencies struct {
	DB           *sql.DB
	ADB          *adb.Client
	Source       Source
	Captures     *capture.Manager
	Transport    *rtc.Endpoint
	Events       *events.Bus
	TaskOwner    func(string) string
	AdminVersion string
	Logger       *slog.Logger
}

type VideoParameters struct {
	MaxSize int `json:"maxSize"`
	MaxFPS  int `json:"maxFps"`
	BitRate int `json:"bitRate"`
}

func (p VideoParameters) capture(revision int64) capture.Settings {
	return capture.Settings{MaxSize: p.MaxSize, MaxFPS: p.MaxFPS, BitRate: p.BitRate, Revision: revision}
}

type EffectiveVideo struct {
	Width                uint16  `json:"width"`
	Height               uint16  `json:"height"`
	FramesPerSecond      float64 `json:"framesPerSecond"`
	EncodedBitsPerSecond int64   `json:"encodedBitsPerSecond"`
	Revision             int64   `json:"revision"`
}

type VideoSnapshot struct {
	Requested    VideoParameters `json:"requested"`
	Effective    *EffectiveVideo `json:"effective"`
	Status       string          `json:"status"`
	Revision     int64           `json:"revision"`
	CaptureEpoch uint64          `json:"captureEpoch"`
	ErrorCode    string          `json:"errorCode,omitempty"`
}

type VideoPatch struct {
	SessionID string `json:"sessionId,omitempty"`
	Revision  int64  `json:"revision"`
	MaxSize   *int   `json:"maxSize,omitempty"`
	MaxFPS    *int   `json:"maxFps,omitempty"`
	BitRate   *int   `json:"bitRate,omitempty"`
}

type SessionSnapshot struct {
	SessionID       string `json:"sessionId"`
	DeviceID        string `json:"deviceId"`
	Mode            string `json:"mode"`
	Answer          string `json:"answer"`
	ControlProtocol string `json:"controlProtocol"`
}

type Manager struct {
	deps            Dependencies
	ctx             context.Context
	cancel          context.CancelFunc
	mu              sync.Mutex
	closed          bool
	sessions        map[string]*session
	hubs            map[string]*hub
	retained        *retainedSession
	watcher         sync.WaitGroup
	screenshotSlots chan struct{}
}

type session struct {
	manager                                                 *Manager
	hub                                                     *hub
	id, owner, mode                                         string
	ctx                                                     context.Context
	cancel                                                  context.CancelFunc
	created                                                 time.Time
	retention                                               *retainedSession // guarded by manager.mu
	suspended                                               bool
	interactionCtx                                          context.Context
	cancelInteraction                                       context.CancelFunc
	mu                                                      sync.Mutex
	peer                                                    *rtc.Peer
	peerStarted                                             time.Time
	peerReady                                               chan struct{}
	peerReadyClosed                                         bool
	sequence                                                uint64
	readyEpoch                                              uint64
	boundaryTimestamp                                       uint32
	frameTimestamps                                         [maxFrameBoundaries]uint32
	frameTimestampCount, frameTimestampNext                 int
	frameGeneration                                         uint64
	keyNoticeSent                                           bool
	awaitingKey                                             bool
	waitingSince                                            time.Time
	startupKeyAt                                            time.Time
	frameGenerations                                        map[*videoFrame]uint64
	ready                                                   bool
	queuedFrameBytes, queuedControlBytes, queuedOutputBytes int
	frames                                                  chan *videoFrame
	commands                                                chan []byte
	output                                                  chan []byte
	endOnce                                                 sync.Once
	done                                                    chan struct{}
	endErr                                                  error
	workers                                                 sync.WaitGroup
}

type videoFrame struct {
	packets       []*rtp.Packet
	size          int
	source        *capture.Stream
	epoch         uint64
	width, height uint16
	timestamp     uint32
	key           bool
	observedAt    time.Time
}

type hub struct {
	manager  *Manager
	deviceID string
	// No network or database calls while mu is held. changeMu serializes capture
	// replacement/configuration; inputMu serializes input ownership and dispatch.
	changeMu                          sync.Mutex
	inputMu                           sync.Mutex
	mu                                sync.Mutex
	members                           map[string]*session
	source                            *capture.Stream
	cleanupSource                     *capture.Stream
	requested                         VideoParameters
	revision                          int64
	captureRevision                   int64
	cleanupPending                    bool
	epoch                             uint64
	status, errorCode                 string
	effective                         *EffectiveVideo
	width, height                     uint16
	inputs                            inputState
	config                            []byte
	packetizer                        rtp.Packetizer
	timestampBase, lastTimestamp      uint32
	firstPTS, lastPTS                 uint64
	ptsSet                            bool
	lastFrameAt                       time.Time
	lastKeyFrameAt                    time.Time
	statisticsAt                      time.Time
	statisticsFrames, statisticsBytes int64
	waitingSession                    bool
	sourceReady                       chan struct{}
	sourceReadyClosed                 bool
	clipboardRead                     *clipboardRequest
	clipboardWrites                   map[uint64]*clipboardRequest
	nextClipboardSequence             uint64
}

type clipboardRequest struct {
	session         *session
	controlSequence uint64
	nativeSequence  uint64
	timer           *time.Timer
	tombstone       bool
	dispatching     bool
	acked           bool
	text            string
}

// Scope never grants writes through a readonly session. Its checks remain bound
// to the original session and link even when used by a long file/screenshot job.
type Scope struct {
	Close    func()
	DeviceID string
	Link     leases.Link
	Context  context.Context
	Check    func() error
}

type Screenshot struct {
	Reader        io.ReadCloser
	ContentType   string
	Size          int64
	Width, Height int
}
