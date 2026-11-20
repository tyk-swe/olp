package realtime

import "sync"

type callRegistry struct {
	mu       sync.Mutex
	sessions map[string]*webrtcSession
}

func (r *callRegistry) register(sess *webrtcSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = map[string]*webrtcSession{}
	}
	r.sessions[sess.id] = sess
}

func (r *callRegistry) unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
}
