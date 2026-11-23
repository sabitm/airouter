package web

import (
	"sync"
	"time"

	"airouter/internal/domain"
)

// connectTTL bounds how long an in-flight OAuth connect attempt is kept before
// it is pruned. A connect that is not completed within this window is abandoned;
// the user simply starts over. Kept generous so a slow manual browser login
// (find the tab, approve, paste) still lands inside it.
const connectTTL = 10 * time.Minute

// connector is the shared lifecycle for authorization-code and device-code connects.
type connector interface {
	State() string
	Result() (*domain.OAuthCreds, error, bool)
	Close() error
}

const opencodeConsoleFlow = "opencode-console"
const grokBuildFlow = "grok-build"

// connectSession is one in-flight OAuth connect attempt. It outlives the begin
// request so the later status-poll / paste / save requests can reach the same
// connector, which holds the state tying the flow to the resulting credentials.
// OpenCode Console attempts set bound, flow, and targetID. A save can claim
// that attempt only for the same flow and edit target, and only once per
// successful write. releaseClaim keeps a failed write retryable.
type connectSession struct {
	conn     connector
	created  time.Time
	flow     string
	targetID int64
	bound    bool
	claimed  bool
}

// connectSessions is the web layer's in-memory store of in-flight connect
// attempts, keyed by the connect's state token. It is the first piece of
// per-process state in this package; everything else flows through the store.
// The map is small (one entry per concurrent connect) and guarded by a mutex,
// mirroring the inflight-refresh map in oauth.Service.
type connectSessions struct {
	mu sync.Mutex
	m  map[string]*connectSession
}

func newConnectSessions() *connectSessions {
	return &connectSessions{m: map[string]*connectSession{}}
}

// put stores a session under the given state token, first pruning any expired
// sessions so abandoned attempts (and their loopback servers) do not accumulate.
func (cs *connectSessions) put(state string, sess *connectSession, now time.Time) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for k, s := range cs.m {
		if now.Sub(s.created) > connectTTL {
			s.conn.Close()
			delete(cs.m, k)
		}
	}
	cs.m[state] = sess
}

func (cs *connectSessions) get(state string) (*connectSession, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.getLocked(state)
}

func (cs *connectSessions) getLocked(state string) (*connectSession, bool) {
	s, ok := cs.m[state]
	if ok && s.bound && time.Since(s.created) >= connectTTL {
		delete(cs.m, state)
		s.conn.Close()
		return nil, false
	}
	return s, ok
}

// putBound stores a flow-and-target-bound attempt. targetID 0 is a create form.
func (cs *connectSessions) putBound(state string, conn connector, flow string, targetID int64, now time.Time) {
	cs.put(state, &connectSession{
		conn: conn, created: now, flow: flow, targetID: targetID, bound: true,
	}, now)
}

// claimOpencodeConsole returns a copy of a completed Console attempt when the
// flow and edit target match and the attempt has not already been claimed.
func (cs *connectSessions) claimOpencodeConsole(state string, targetID int64) (*domain.OAuthCreds, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	s, ok := cs.getLocked(state)
	if !ok || s.claimed || !s.bound || s.flow != opencodeConsoleFlow || s.targetID != targetID {
		return nil, false
	}
	creds, err, done := s.conn.Result()
	if !done || err != nil || creds == nil || !creds.OpencodeConsoleAuth || creds.AccessToken == "" || creds.OpencodeOrgID == "" {
		return nil, false
	}
	s.claimed = true
	cp := *creds
	return &cp, true
}

// claimGrokBuild returns a copy of a completed Grok Build attempt when the
// flow and edit target match and the attempt has not already been claimed.
func (cs *connectSessions) claimGrokBuild(state string, targetID int64) (*domain.OAuthCreds, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	s, ok := cs.getLocked(state)
	if !ok || s.claimed || !s.bound || s.flow != grokBuildFlow || s.targetID != targetID {
		return nil, false
	}
	creds, err, done := s.conn.Result()
	if !done || err != nil || creds == nil || !creds.GrokBuildAuth || creds.AccessToken == "" || creds.RefreshToken == "" {
		return nil, false
	}
	s.claimed = true
	cp := *creds
	return &cp, true
}

// peekGrokBuild reads a completed Grok Build attempt without claiming it.
func (cs *connectSessions) peekGrokBuild(state string, targetID int64) (*domain.OAuthCreds, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	s, ok := cs.getLocked(state)
	if !ok || s.claimed || !s.bound || s.flow != grokBuildFlow || s.targetID != targetID {
		return nil, false
	}
	creds, err, done := s.conn.Result()
	if !done || err != nil || creds == nil || !creds.GrokBuildAuth || creds.AccessToken == "" || creds.RefreshToken == "" {
		return nil, false
	}
	cp := *creds
	return &cp, true
}

// peekOpencodeConsole reads a completed Console attempt without claiming it.
func (cs *connectSessions) peekOpencodeConsole(state string, targetID int64) (*domain.OAuthCreds, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	s, ok := cs.getLocked(state)
	if !ok || s.claimed || !s.bound || s.flow != opencodeConsoleFlow || s.targetID != targetID {
		return nil, false
	}
	creds, err, done := s.conn.Result()
	if !done || err != nil || creds == nil || !creds.OpencodeConsoleAuth || creds.AccessToken == "" || creds.OpencodeOrgID == "" {
		return nil, false
	}
	cp := *creds
	return &cp, true
}

// releaseClaim clears a claim so a failed save can be retried. It does not
// drop the attempt.
func (cs *connectSessions) releaseClaim(state string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if s, ok := cs.m[state]; ok {
		s.claimed = false
	}
}

// drop removes and closes the session for a state token. Idempotent.
func (cs *connectSessions) drop(state string) {
	cs.mu.Lock()
	s, ok := cs.m[state]
	delete(cs.m, state)
	cs.mu.Unlock()
	if ok {
		s.conn.Close()
	}
}
