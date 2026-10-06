package kirocatalog

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
)

type errAfterReader struct {
	data []byte
	err  error
	off  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	if r.off >= len(r.data) {
		return n, r.err
	}
	return n, nil
}

func TestLegacyStatusAndTruncationWinOverBodyRead(t *testing.T) {
	readErr := errors.New("body read failed after status")
	var calls atomic.Int32
	var refreshed atomic.Int32
	c := &Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			return &http.Response{
				StatusCode:    http.StatusUnauthorized,
				Header:        make(http.Header),
				Body:          io.NopCloser(&errAfterReader{data: []byte(`{"message":"old"}`), err: readErr}),
				ContentLength: -1,
			}, nil
		}
		return jsonResponse(http.StatusOK, catalogJSON("fresh")), nil
	})}}
	p := &domain.Provider{BaseURL: "https://catalog.example", APIKey: "old", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	models, err := c.ListWithRefresh(context.Background(), nil, p, func(context.Context) (string, error) {
		refreshed.Add(1)
		return "new", nil
	})
	if err != nil || refreshed.Load() != 1 || calls.Load() != 2 || len(models) != 1 || models[0].ID != "fresh" {
		t.Fatalf("legacy 401 body-read hid auth models=%v calls=%d refresh=%d err=%v", models, calls.Load(), refreshed.Load(), err)
	}

	calls.Store(0)
	c = &Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body := strings.Repeat("x", captureMax+8)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(&errAfterReader{data: []byte(body), err: readErr}),
		}, nil
	})}}
	if _, err := c.List(context.Background(), nil, p); !errors.Is(err, ErrTruncated) || calls.Load() != 1 {
		t.Fatalf("legacy truncation hidden by body read calls=%d err=%v truncated=%v", calls.Load(), err, errors.Is(err, ErrTruncated))
	}
	body, total, truncated, err := readBounded(&errAfterReader{data: []byte(strings.Repeat("x", captureMax)), err: readErr}, captureMax)
	if !errors.Is(err, readErr) || truncated || len(body) != captureMax || total != int64(captureMax) {
		t.Fatalf("reader body=%d total=%d truncated=%v err=%v", len(body), total, truncated, err)
	}
}

func TestListWithRefreshPreservesShorterDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	deadline, ok := parent.Deadline()
	if !ok {
		t.Fatal("missing parent deadline")
	}
	c := &Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got, childOK := r.Context().Deadline()
		if !childOK || !got.Equal(deadline) {
			t.Errorf("child deadline reset got=%v ok=%v", got, childOK)
		}
		return jsonResponse(http.StatusOK, catalogJSON("m")), nil
	})}}
	p := &domain.Provider{BaseURL: "https://catalog.example", APIKey: "k", Protocol: domain.ProtocolKiro}
	if _, err := c.ListWithRefresh(parent, nil, p, nil); err != nil {
		t.Fatal(err)
	}
}
