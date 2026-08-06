package harlog

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestControllerRuntimeLifecycle(t *testing.T) {
	c := NewController(false, "v", discardLogger())
	if c.Mode() != ModeRuntime {
		t.Fatalf("mode = %v", c.Mode())
	}
	st := c.Status()
	if st.State != StateIdle || st.InFlight != 0 {
		t.Fatalf("idle status = %+v", st)
	}
	if c.Acquire() != nil {
		t.Fatal("idle acquire should be nil")
	}
	if _, err := c.Download(); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("idle download = %v", err)
	}
	if err := c.Stop(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stop idle = %v", err)
	}

	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	st = c.Status()
	if st.State != StateRecording || st.StartedAt.IsZero() {
		t.Fatalf("recording status = %+v", st)
	}
	if _, err := c.Download(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("recording download = %v", err)
	}
	if err := c.Start(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("start while recording = %v", err)
	}

	l1 := c.Acquire()
	if l1 == nil || l1.Recorder() == nil {
		t.Fatal("expected lease")
	}
	l1.Recorder().EnsurePage("page_a", "t", time.Now())
	l1.Recorder().Record(RecordInput{
		PageID: "page_a", Method: "GET", URL: "http://x", Status: 200, StartedAt: time.Now(),
	})
	if st := c.Status(); st.InFlight != 1 || st.Pages != 1 || st.Entries != 1 {
		t.Fatalf("in-flight status = %+v", st)
	}

	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	st = c.Status()
	if st.State != StateStopping || st.InFlight != 1 {
		t.Fatalf("stopping status = %+v", st)
	}
	if c.Acquire() != nil {
		t.Fatal("acquire after stop should be nil")
	}
	if _, err := c.Download(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stopping download = %v", err)
	}
	if err := c.Start(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("start while stopping = %v", err)
	}
	if err := c.Stop(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("double stop = %v", err)
	}

	l1.Release()
	l1.Release() // exactly once
	st = c.Status()
	if st.State != StateStopped || st.InFlight != 0 || st.StoppedAt.IsZero() {
		t.Fatalf("stopped status = %+v", st)
	}
	data, err := c.Download()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	// Start replaces stopped recorder.
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	st = c.Status()
	if st.State != StateRecording || st.Entries != 0 || st.Pages != 0 || !st.StoppedAt.IsZero() {
		t.Fatalf("fresh start status = %+v", st)
	}
	// Immediate stop with no in-flight goes Stopped.
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); st.State != StateStopped {
		t.Fatalf("empty stop = %+v", st)
	}
	if err := c.Stop(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stop stopped = %v", err)
	}
}

func TestControllerFileMode(t *testing.T) {
	c := NewController(true, "fv", discardLogger())
	if c.Mode() != ModeFile {
		t.Fatalf("mode = %v", c.Mode())
	}
	if st := c.Status(); st.State != StateRecording || st.Mode != ModeFile {
		t.Fatalf("status = %+v", st)
	}
	if err := c.Start(); !errors.Is(err, ErrFileMode) {
		t.Fatalf("Start = %v", err)
	}
	if err := c.Stop(); !errors.Is(err, ErrFileMode) {
		t.Fatalf("Stop = %v", err)
	}
	l := c.Acquire()
	if l == nil {
		t.Fatal("file acquire nil")
	}
	l.Recorder().EnsurePage("page_f", "t", time.Now())
	l.Release()
	data, err := c.Download()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty download")
	}
	if c.FileRecorder() == nil {
		t.Fatal("FileRecorder nil")
	}
	// Runtime controller has no file recorder.
	rt := NewController(false, "v", discardLogger())
	if rt.FileRecorder() != nil {
		t.Fatal("runtime FileRecorder should be nil")
	}
}

func TestControllerDownloadDoesNotBlockAcquire(t *testing.T) {
	c := NewController(true, "file", discardLogger())
	rec := c.FileRecorder()
	rec.mu.Lock()

	downloadStarted := make(chan struct{})
	downloadDone := make(chan error, 1)
	go func() {
		close(downloadStarted)
		_, err := c.Download()
		downloadDone <- err
	}()
	<-downloadStarted
	// Give Download time to select the recorder and block on its mutex.
	time.Sleep(20 * time.Millisecond)

	acquireDone := make(chan *Lease, 1)
	go func() { acquireDone <- c.Acquire() }()
	select {
	case lease := <-acquireDone:
		if lease == nil {
			t.Fatal("file acquire nil")
		}
		lease.Release()
	case <-time.After(500 * time.Millisecond):
		rec.mu.Unlock()
		t.Fatal("live download blocked request acquisition")
	}

	rec.mu.Unlock()
	if err := <-downloadDone; err != nil {
		t.Fatalf("Download: %v", err)
	}
}

func TestControllerConcurrentAcquireStop(t *testing.T) {
	c := NewController(false, "race", discardLogger())
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			l := c.Acquire()
			if l == nil {
				return
			}
			l.Recorder().EnsurePage("page_x", "t", time.Now())
			l.Recorder().Record(RecordInput{
				PageID: "page_x", Method: "POST", URL: "http://x", Status: 200, StartedAt: time.Now(),
			})
			time.Sleep(time.Millisecond)
			l.Release()
			l.Release()
		}()
	}
	// Concurrent status/download while recording.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			_ = c.Status()
			_, _ = c.Download()
		}
	}()
	time.Sleep(2 * time.Millisecond)
	if err := c.Stop(); err != nil && !errors.Is(err, ErrInvalidTransition) {
		// Stop may race with a prior Stop only if called twice; single call ok.
		t.Fatalf("Stop: %v", err)
	}
	wg.Wait()
	<-done
	// Drain if still stopping.
	deadline := time.Now().Add(2 * time.Second)
	for c.Status().State == StateStopping && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	st := c.Status()
	if st.State != StateStopped {
		t.Fatalf("final state = %+v", st)
	}
	if st.InFlight != 0 {
		t.Fatalf("inFlight = %d", st.InFlight)
	}
	if _, err := c.Download(); err != nil {
		t.Fatalf("download: %v", err)
	}
}

func TestRecorderStats(t *testing.T) {
	r := New("s")
	if p, e := r.Stats(); p != 0 || e != 0 {
		t.Fatalf("empty stats %d %d", p, e)
	}
	r.EnsurePage("page_1", "a", time.Now())
	r.Record(RecordInput{PageID: "page_1", Method: "GET", URL: "http://a", Status: 200, StartedAt: time.Now()})
	r.Record(RecordInput{PageID: "page_1", Method: "GET", URL: "http://b", Status: 200, StartedAt: time.Now()})
	p, e := r.Stats()
	if p != 1 || e != 2 {
		t.Fatalf("stats pages=%d entries=%d", p, e)
	}
	var nilR *Recorder
	if p, e := nilR.Stats(); p != 0 || e != 0 {
		t.Fatalf("nil stats %d %d", p, e)
	}
}
