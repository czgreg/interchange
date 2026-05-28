// Package configstore handles atomic write-back of /etc/leap/gateway.yaml from
// the in-memory *config.Config — used by the management API when an operator
// edits whitelist or subscriptions through HTTP.
//
// Round-tripping via yaml.Marshal does NOT preserve YAML comments — the file
// after write looks valid but loses the human-authored notes. The user
// accepted this tradeoff (see API design discussion); operators who care
// about comments should hand-edit and call POST /api/subscribe/refresh.
package configstore

import (
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// Store is a serialized writer that owns the on-disk config file. Concurrent
// API calls are safe: each Mutate runs under the mutex so two PUTs can't
// step on each other and produce a half-merged yaml.
type Store struct {
	path string
	mu   sync.Mutex
}

func New(path string) *Store {
	return &Store{path: path}
}

// Path returns the on-disk config path the store is bound to.
func (s *Store) Path() string { return s.path }

// Mutate runs fn against the in-memory *cfg, then atomically writes the
// result back to disk. The caller is expected to mutate cfg in place — this
// indirection exists so the lock covers both the mutation AND the write,
// which is what makes concurrent API edits safe.
//
// On write failure, cfg is left in its mutated state — the caller is
// responsible for any rollback they want. (For our API, the cfg lives in
// process memory, so a failed write means in-memory and on-disk diverge
// until the next successful write or process restart. We accept that.)
func (s *Store) Mutate(cfg *config.Config, fn func(*config.Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(cfg); err != nil {
		return err
	}
	return s.writeAtomic(cfg)
}

func (s *Store) writeAtomic(cfg *config.Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("yaml.Marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename %s: %w", s.path, err)
	}
	return nil
}
