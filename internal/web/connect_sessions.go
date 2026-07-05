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

// connectSession is one in-flight OAuth connect attempt. It outlives the begin
// request so the later status-poll / paste / save requests can reach the same
// connector, which holds the state tying the flow to the resulting credentials.
type connectSession struct {
	conn    connector
	created time.Time
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
	s, ok := cs.m[state]
	return s, ok
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