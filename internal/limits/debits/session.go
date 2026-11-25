package debits

type Session struct {
	*Store
	*Syncer
}

func NewSession(store *Store, syncer *Syncer) *Session {
	return &Session{Store: store, Syncer: syncer}
}
