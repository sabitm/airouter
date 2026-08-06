// Package harlog records proxied HTTP exchanges as a HAR 1.2 document that
// Chrome DevTools can import. Capture is verbatim: no header or body redaction.
package harlog

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// MaxEntries is the in-memory ring capacity for HAR entries. When full, the
// oldest entry is dropped.
const MaxEntries = 2000

// MaxBody is the per-body capture cap (request postData and response content).
// Bodies larger than this are truncated and annotated via the comment field.
const MaxBody = 1 << 20 // 1 MiB

// Recorder accumulates HAR pages and entries under a mutex. A nil *Recorder is
// treated as disabled by callers; methods on a live recorder are safe for
// concurrent use.
type Recorder struct {
	mu      sync.Mutex
	version string
	pages   []harPage
	pageIDs map[string]struct{}
	entries []harEntry
}

// New returns an empty recorder. creatorVersion is written into log.creator.
func New(creatorVersion string) *Recorder {
	if creatorVersion == "" {
		creatorVersion = "dev"
	}
	return &Recorder{
		version: creatorVersion,
		pageIDs: map[string]struct{}{},
	}
}

// EnsurePage registers a page with the given id if one is not already present.
// pageID is typically "page_<RequestID>" so ingress and upstream legs share it.
func (r *Recorder) EnsurePage(pageID, title string, startedAt time.Time) {
	if r == nil || pageID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pageIDs[pageID]; ok {
		return
	}
	if title == "" {
		title = pageID
	}
	r.pageIDs[pageID] = struct{}{}
	r.pages = append(r.pages, harPage{
		StartedDateTime: formatTime(startedAt),
		ID:              pageID,
		Title:           title,
		PageTimings: harPageTimings{
			OnContentLoad: -1,
			OnLoad:        -1,
		},
	})
	if len(r.pages) > MaxEntries {
		old := r.pages[0].ID
		r.pages = r.pages[1:]
		delete(r.pageIDs, old)
	}
}

// RecordInput is the capture payload for one HTTP leg (ingress or upstream).
type RecordInput struct {
	PageID      string
	StartedAt   time.Time
	Duration    time.Duration
	Method      string
	URL         string
	ReqHeaders  http.Header
	ReqBody     []byte
	Status      int
	RespHeaders http.Header
	RespBody    []byte
	// RespMIME, when non-empty, overrides Content-Type for response content.
	RespMIME string
	// ReqBodySize/RespBodySize, when > len of the corresponding body slice,
	// record the original wire length (used when a stream tee already truncated).
	ReqBodySize  int
	RespBodySize int
}

// Record appends one HAR entry. Bodies over MaxBody are truncated in place and
// annotated. No-op on a nil receiver or empty PageID.
func (r *Recorder) Record(in RecordInput) {
	if r == nil || in.PageID == "" {
		return
	}
	waitMS := float64(in.Duration) / float64(time.Millisecond)
	if waitMS < 0 {
		waitMS = 0
	}

	reqOrig := len(in.ReqBody)
	if in.ReqBodySize > reqOrig {
		reqOrig = in.ReqBodySize
	}
	respOrig := len(in.RespBody)
	if in.RespBodySize > respOrig {
		respOrig = in.RespBodySize
	}
	reqBody, reqComment := clipBody(in.ReqBody, reqOrig)
	respBody, respComment := clipBody(in.RespBody, respOrig)

	reqMIME := headerMIME(in.ReqHeaders)
	if reqMIME == "" {
		reqMIME = "application/json"
	}
	respMIME := in.RespMIME
	if respMIME == "" {
		respMIME = headerMIME(in.RespHeaders)
	}
	if respMIME == "" {
		respMIME = "application/json"
	}

	entry := harEntry{
		StartedDateTime: formatTime(in.StartedAt),
		Time:            waitMS,
		PageRef:         in.PageID,
		Request: harRequest{
			Method:      in.Method,
			URL:         in.URL,
			HTTPVersion: "HTTP/1.1",
			Headers:     headersFrom(in.ReqHeaders),
			QueryString: queryFromURL(in.URL),
			Cookies:     []nameValue{},
			HeadersSize: -1,
			BodySize:    int64(reqOrig),
			Comment:     reqComment,
		},
		Response: harResponse{
			Status:      in.Status,
			StatusText:  http.StatusText(in.Status),
			HTTPVersion: "HTTP/1.1",
			Headers:     headersFrom(in.RespHeaders),
			Cookies:     []nameValue{},
			Content: harContent{
				Size:     int64(respOrig),
				MimeType: respMIME,
				Text:     string(respBody),
				Comment:  respComment,
			},
			RedirectURL: "",
			HeadersSize: -1,
			BodySize:    int64(respOrig),
			Comment:     respComment,
		},
		Cache: harCache{},
		Timings: harTimings{
			Blocked: -1,
			DNS:     -1,
			Connect: -1,
			SSL:     -1,
			Send:    0,
			Wait:    waitMS,
			Receive: 0,
		},
	}
	if len(reqBody) > 0 || reqComment != "" {
		entry.Request.PostData = &harPostData{
			MimeType: reqMIME,
			Text:     string(reqBody),
			Comment:  reqComment,
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry)
	if len(r.entries) > MaxEntries {
		r.entries = r.entries[1:]
	}
}

// Stats returns the current page and entry counts under the recorder mutex.
func (r *Recorder) Stats() (pages, entries int) {
	if r == nil {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pages), len(r.entries)
}

// MarshalHAR returns a pretty-printed HAR 1.2 document.
func (r *Recorder) MarshalHAR() ([]byte, error) {
	if r == nil {
		return json.MarshalIndent(harDoc{Log: harLog{
			Version: "1.2",
			Creator: harCreator{Name: "airouter", Version: "dev"},
			Pages:   []harPage{},
			Entries: []harEntry{},
		}}, "", "  ")
	}
	r.mu.Lock()
	pages := append([]harPage(nil), r.pages...)
	entries := append([]harEntry(nil), r.entries...)
	version := r.version
	r.mu.Unlock()
	if pages == nil {
		pages = []harPage{}
	}
	if entries == nil {
		entries = []harEntry{}
	}
	// Records are immutable after append, so the shallow slice snapshots remain
	// stable while JSON encoding proceeds without blocking capture writes.
	doc := harDoc{Log: harLog{
		Version: "1.2",
		Creator: harCreator{Name: "airouter", Version: version},
		Pages:   pages,
		Entries: entries,
	}}
	return json.MarshalIndent(doc, "", "  ")
}

// WriteFile writes the current HAR document to path via a temp file + rename.
func (r *Recorder) WriteFile(path string) error {
	data, err := r.MarshalHAR()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// clipBody returns at most MaxBody bytes of b. orig is the full wire length
// (may exceed len(b) when the caller already truncated).
func clipBody(b []byte, orig int) (clipped []byte, comment string) {
	if orig < len(b) {
		orig = len(b)
	}
	if len(b) > MaxBody {
		b = b[:MaxBody]
	}
	if b == nil {
		if orig > MaxBody {
			return nil, "truncated from " + itoa(orig) + " bytes"
		}
		return nil, ""
	}
	out := make([]byte, len(b))
	copy(out, b)
	if orig > len(out) {
		return out, "truncated from " + itoa(orig) + " bytes"
	}
	return out, ""
}

func headersFrom(h http.Header) []nameValue {
	if len(h) == 0 {
		return []nameValue{}
	}
	out := make([]nameValue, 0, len(h))
	for name, vals := range h {
		for _, v := range vals {
			out = append(out, nameValue{Name: name, Value: v})
		}
	}
	return out
}

func queryFromURL(raw string) []nameValue {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return []nameValue{}
	}
	q := u.Query()
	out := make([]nameValue, 0, len(q))
	for k, vs := range q {
		for _, v := range vs {
			out = append(out, nameValue{Name: k, Value: v})
		}
	}
	return out
}

func headerMIME(h http.Header) string {
	if h == nil {
		return ""
	}
	return h.Get("Content-Type")
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	// RFC3339 with millis (HAR 1.2 / Chrome).
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// HAR 1.2 wire types. Field names match the spec / Chrome DevTools import.

type harDoc struct {
	Log harLog `json:"log"`
}

type harLog struct {
	Version string     `json:"version"`
	Creator harCreator `json:"creator"`
	Pages   []harPage  `json:"pages"`
	Entries []harEntry `json:"entries"`
}

type harCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type harPage struct {
	StartedDateTime string         `json:"startedDateTime"`
	ID              string         `json:"id"`
	Title           string         `json:"title"`
	PageTimings     harPageTimings `json:"pageTimings"`
}

type harPageTimings struct {
	OnContentLoad float64 `json:"onContentLoad"`
	OnLoad        float64 `json:"onLoad"`
}

type harEntry struct {
	StartedDateTime string      `json:"startedDateTime"`
	Time            float64     `json:"time"`
	Request         harRequest  `json:"request"`
	Response        harResponse `json:"response"`
	Cache           harCache    `json:"cache"`
	Timings         harTimings  `json:"timings"`
	PageRef         string      `json:"pageref"`
}

type harRequest struct {
	Method      string       `json:"method"`
	URL         string       `json:"url"`
	HTTPVersion string       `json:"httpVersion"`
	Headers     []nameValue  `json:"headers"`
	QueryString []nameValue  `json:"queryString"`
	Cookies     []nameValue  `json:"cookies"`
	HeadersSize int          `json:"headersSize"`
	BodySize    int64        `json:"bodySize"`
	PostData    *harPostData `json:"postData,omitempty"`
	Comment     string       `json:"comment,omitempty"`
}

type harPostData struct {
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
	Comment  string `json:"comment,omitempty"`
}

type harResponse struct {
	Status      int         `json:"status"`
	StatusText  string      `json:"statusText"`
	HTTPVersion string      `json:"httpVersion"`
	Headers     []nameValue `json:"headers"`
	Cookies     []nameValue `json:"cookies"`
	Content     harContent  `json:"content"`
	RedirectURL string      `json:"redirectURL"`
	HeadersSize int         `json:"headersSize"`
	BodySize    int64       `json:"bodySize"`
	Comment     string      `json:"comment,omitempty"`
}

type harContent struct {
	Size     int64  `json:"size"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
	Comment  string `json:"comment,omitempty"`
}

type harCache struct{}

type harTimings struct {
	Blocked float64 `json:"blocked"`
	DNS     float64 `json:"dns"`
	Connect float64 `json:"connect"`
	SSL     float64 `json:"ssl"`
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}

type nameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
