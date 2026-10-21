//go:build integration

package integration_test

import (
	"net/http/httptest"
	"sync"
)

type nativeScopeCall struct {
	account string
	method  string
	path    string
}

type nativeScopeFixture struct {
	*httptest.Server
	mu    sync.Mutex
	calls []nativeScopeCall
}

func (f *nativeScopeFixture) captured() []nativeScopeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]nativeScopeCall(nil), f.calls...)
}
