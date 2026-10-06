package cli

import (
	"encoding/json"
	"sync"
)

// tokenBox holds the live credentials: the runner's goroutines read the
// token on every request while the renewal timer replaces it.
type tokenBox struct {
	mu    sync.RWMutex
	creds *Credentials
}

func newTokenBox(c *Credentials) *tokenBox { return &tokenBox{creds: c} }

func (b *tokenBox) get() *Credentials {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.creds
}

func (b *tokenBox) set(c *Credentials) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.creds = c
}

func (b *tokenBox) token() string { return b.get().Token }

// jsonLine renders log detail as compact JSON (keys sorted, like a stable
// log line). Values the runner logs are always marshalable.
func jsonLine(extra map[string]any) string {
	raw, err := json.Marshal(extra)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
