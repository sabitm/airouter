package harlog

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Mode is fixed at controller construction: file-backed always-on capture, or
// dashboard-driven runtime sessions.
type Mode int

const (
	// ModeRuntime starts Idle; capture is manually started and stopped.
	ModeRuntime Mode = iota
	// ModeFile captures from process start and rejects Start/Stop.
	ModeFile
)

// State is the runtime session lifecycle. File mode reports StateRecording.
type State int

const (
	StateIdle State = iota
	StateRecording
	StateStopping
	StateStopped
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateRecording:
		return "recording"
	case StateStopping:
		return "stopping"
	case StateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

func (m Mode) String() string {
	switch m {
	case ModeFile:
		return "file"
	default:
		return "runtime"
	}
}

var (
	// ErrInvalidTransition is returned for illegal Start/Stop transitions.
	ErrInvalidTransition = errors.New("har: invalid transition")
	// ErrFileMode is returned when Start/Stop is attempted under ModeFile.
	ErrFileMode = errors.New("har: controls disabled in file mode")
	// ErrNotAvailable means there is no downloadable document (runtime Idle).
	ErrNotAvailable = errors.New("har: not available")
	// ErrNotReady means a document exists but is not stable yet (Recording/Stopping).
	ErrNotReady = errors.New("har: not ready")
)

// Status is a snapshot of controller state for the dashboard and tests.
type Status struct {
	Mode      Mode
	State     State
	StartedAt time.Time
	StoppedAt time.Time
	Pages     int
	Entries   int
	InFlight  int
}

// Controller owns process-wide HAR capture mode and the active/retained session.
// A request must Acquire once at ingress and pin the returned recorder for both
// legs; Release runs exactly once after the ingress entry is written.
type Controller struct {
	mode    Mode
	version string
	logger  *slog.Logger

	mu        sync.Mutex
	state     State
	rec       *Recorder
	inFlight  int
	startedAt time.Time
	stoppedAt time.Time
}

// NewController builds a process-wide owner. fileMode selects ModeFile (always
// recording) versus ModeRuntime (dashboard Start/Stop). logger may be nil.
func NewController(fileMode bool, creatorVersion string, logger *slog.Logger) *Controller {
	if creatorVersion == "" {
		creatorVersion = "dev"
	}
	if logger == nil {
		logger = slog.Default()
	}
	c := &Controller{
		version: creatorVersion,
		logger:  logger,
	}
	if fileMode {
		c.mode = ModeFile
		c.state = StateRecording
		c.rec = New(creatorVersion)
		c.startedAt = time.Now()
		return c
	}
	c.mode = ModeRuntime
	c.state = StateIdle
	return c
}

// Mode reports the immutable capture mode.
func (c *Controller) Mode() Mode {
	if c == nil {
		return ModeRuntime
	}
	return c.mode
}

// FileRecorder returns the always-on recorder in ModeFile, else nil. Used by
// shutdown to flush -har-file; runtime mode never exposes a shutdown writer.
func (c *Controller) FileRecorder() *Recorder {
	if c == nil || c.mode != ModeFile {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rec
}

// Start begins a new runtime recording session, replacing any retained Stopped
// recorder. Rejected while already Recording/Stopping or in file mode.
func (c *Controller) Start() error {
	if c == nil {
		return ErrInvalidTransition
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode == ModeFile {
		return ErrFileMode
	}
	switch c.state {
	case StateIdle, StateStopped:
		// ok
	default:
		return ErrInvalidTransition
	}
	c.rec = New(c.version)
	c.state = StateRecording
	c.inFlight = 0
	c.startedAt = time.Now()
	c.stoppedAt = time.Time{}
	c.logger.Info("har_runtime_started",
		"event", "har_runtime_started",
		"started_at", c.startedAt.UTC().Format(time.RFC3339),
	)
	return nil
}

// Stop ends acquisition of new requests. Already-acquired leases finish under
// Stopping; the final Release transitions to Stopped. Rejected when not
// Recording or in file mode.
func (c *Controller) Stop() error {
	if c == nil {
		return ErrInvalidTransition
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode == ModeFile {
		return ErrFileMode
	}
	if c.state != StateRecording {
		return ErrInvalidTransition
	}
	c.state = StateStopping
	c.logger.Info("har_runtime_stopping",
		"event", "har_runtime_stopping",
		"in_flight", c.inFlight,
	)
	if c.inFlight == 0 {
		c.finishLocked()
	}
	return nil
}

func (c *Controller) finishLocked() {
	c.state = StateStopped
	c.stoppedAt = time.Now()
	pages, entries := 0, 0
	if c.rec != nil {
		pages, entries = c.rec.Stats()
	}
	c.logger.Info("har_runtime_stopped",
		"event", "har_runtime_stopped",
		"pages", pages,
		"entries", entries,
		"started_at", c.startedAt.UTC().Format(time.RFC3339),
		"stopped_at", c.stoppedAt.UTC().Format(time.RFC3339),
	)
}

// Status returns a consistent snapshot for UI and tests.
func (c *Controller) Status() Status {
	if c == nil {
		return Status{Mode: ModeRuntime, State: StateIdle}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{
		Mode:      c.mode,
		State:     c.state,
		StartedAt: c.startedAt,
		StoppedAt: c.stoppedAt,
		InFlight:  c.inFlight,
	}
	if c.rec != nil {
		st.Pages, st.Entries = c.rec.Stats()
	}
	return st
}

// Acquire pins the active recorder for one request. Returns nil when capture is
// off (runtime Idle/Stopping/Stopped). The caller must Release the lease exactly
// once after finishing both legs' ingress recording.
func (c *Controller) Acquire() *Lease {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.mode {
	case ModeFile:
		if c.rec == nil {
			return nil
		}
	default:
		if c.state != StateRecording || c.rec == nil {
			return nil
		}
	}
	c.inFlight++
	return &Lease{ctrl: c, rec: c.rec}
}

// Lease is a single-request pin on a recorder. Release is idempotent.
type Lease struct {
	ctrl     *Controller
	rec      *Recorder
	released atomic.Bool
}

// Recorder returns the pinned recorder (never nil on a non-nil lease).
func (l *Lease) Recorder() *Recorder {
	if l == nil {
		return nil
	}
	return l.rec
}

// Release decrements in-flight exactly once. The final release under Stopping
// transitions the controller to Stopped.
func (l *Lease) Release() {
	if l == nil || l.ctrl == nil {
		return
	}
	if !l.released.CompareAndSwap(false, true) {
		return
	}
	l.ctrl.releaseOne()
}

func (c *Controller) releaseOne() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight > 0 {
		c.inFlight--
	}
	if c.mode == ModeRuntime && c.state == StateStopping && c.inFlight == 0 {
		c.finishLocked()
	}
}

// Download returns a pretty-printed HAR document when download is allowed.
// File mode: live snapshot. Runtime Stopped: retained session. Runtime Idle:
// ErrNotAvailable. Runtime Recording/Stopping: ErrNotReady.
func (c *Controller) Download() ([]byte, error) {
	if c == nil {
		return nil, ErrNotAvailable
	}
	c.mu.Lock()
	var rec *Recorder
	switch c.mode {
	case ModeFile:
		rec = c.rec
		if rec == nil {
			c.mu.Unlock()
			return nil, ErrNotAvailable
		}
	default:
		switch c.state {
		case StateStopped:
			rec = c.rec
			if rec == nil {
				c.mu.Unlock()
				return nil, ErrNotAvailable
			}
		case StateIdle:
			c.mu.Unlock()
			return nil, ErrNotAvailable
		default:
			c.mu.Unlock()
			return nil, ErrNotReady
		}
	}
	c.mu.Unlock()

	// The selected recorder is immutable in runtime Stopped state and permanent
	// in file mode. Marshal without c.mu so a large live export cannot block
	// request acquisition or release.
	return rec.MarshalHAR()
}
