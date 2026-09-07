// Package store is courier's resource storage: an in-memory index over a
// directory of JSON files, with a global resource version, optimistic
// concurrency and a resumable watch.
//
// It is generic over kinds. A kind is anything that implements api.Object, and
// the store never learns what a Message or a Channel means — that belongs to
// pkg/courier. What the store owns is the part every kind needs and every kind
// gets wrong when written twice: assigning names and versions, refusing a write
// made against a stale read, persisting atomically, and telling watchers what
// changed in an order they can resume from.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kvaps/courier/pkg/api"
)

// Store holds every resource. The zero value is not usable; call Open.
type Store struct {
	dir string

	mu      sync.RWMutex
	rv      int64
	objects map[string]map[string]record // kind -> name -> record
	hub     *hub
}

// record is one stored object: its metadata, kept decoded so the store can
// index and version without knowing the kind, and its body as written.
type record struct {
	meta api.ObjectMeta
	raw  json.RawMessage
}

// Open loads a store from dir, creating it if absent. A file that cannot be
// parsed is reported and skipped rather than fatal: one corrupt message must
// not stop the daemon from carrying every other conversation.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("store: empty directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, objects: map[string]map[string]record{}, hub: newHub()}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir is the directory the store persists to.
func (s *Store) Dir() string { return s.dir }

// Version returns the store's current resource version — the point a watch
// started now would begin from.
func (s *Store) Version() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rv
}

func (s *Store) load() error {
	counter := filepath.Join(s.dir, "meta.json")
	if b, err := os.ReadFile(counter); err == nil {
		var m struct {
			ResourceVersion int64 `json:"resourceVersion"`
		}
		if json.Unmarshal(b, &m) == nil {
			s.rv = m.ResourceVersion
		}
	}

	kinds, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, kd := range kinds {
		if !kd.IsDir() {
			continue
		}
		kind := kd.Name()
		entries, err := os.ReadDir(filepath.Join(s.dir, kind))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			path := filepath.Join(s.dir, kind, e.Name())
			b, err := os.ReadFile(path) //nolint:gosec // path built from the store's own directory listing
			if err != nil {
				return err
			}
			var probe struct {
				Metadata api.ObjectMeta `json:"metadata"`
			}
			if err := json.Unmarshal(b, &probe); err != nil {
				// A half-written or hand-edited file. Skipping it loses one
				// object; refusing to start loses the whole daemon.
				fmt.Fprintf(os.Stderr, "store: skipping unreadable %s: %v\n", path, err)
				continue
			}
			s.put(kind, probe.Metadata, b)
			if probe.Metadata.ResourceVersion > s.rv {
				s.rv = probe.Metadata.ResourceVersion
			}
		}
	}
	return nil
}

func (s *Store) put(kind string, meta api.ObjectMeta, raw json.RawMessage) {
	byName, ok := s.objects[kind]
	if !ok {
		byName = map[string]record{}
		s.objects[kind] = byName
	}
	byName[meta.Name] = record{meta: meta, raw: raw}
}

// next allocates the next resource version. Callers must hold the write lock.
//
// The counter is global rather than per-kind so that a single watch stream can
// carry every kind and still be resumable: with per-kind counters a client
// holding one number could not say where it stood across kinds.
func (s *Store) next() int64 {
	s.rv++
	return s.rv
}

func (s *Store) path(kind, name string) string {
	return filepath.Join(s.dir, kind, name+".json")
}

// persist writes an object and the version counter to disk atomically.
func (s *Store) persist(kind, name string, raw []byte) error {
	dir := filepath.Join(s.dir, kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(s.path(kind, name), raw); err != nil {
		return err
	}
	return s.persistCounter()
}

func (s *Store) persistCounter() error {
	b, err := json.Marshal(map[string]int64{"resourceVersion": s.rv})
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, "meta.json"), b)
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// nameSafe rejects names that would escape the store directory or collide with
// its bookkeeping. Names reach the store from MCP callers, so this is a real
// boundary, not a formality.
func nameSafe(name string) error {
	switch {
	case name == "":
		return api.NewInvalid("metadata.name is required")
	case len(name) > 200:
		return api.NewInvalid("metadata.name is too long (max 200)")
	case strings.ContainsAny(name, `/\`), strings.Contains(name, ".."):
		return api.NewInvalid("metadata.name %q may not contain a path separator or %q", name, "..")
	case strings.HasPrefix(name, "."):
		return api.NewInvalid("metadata.name %q may not start with a dot", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return api.NewInvalid("metadata.name may not contain control characters")
		}
	}
	return nil
}

func newUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a condition a local daemon can work
		// around; a duplicate uid would silently confuse deletion history.
		panic("store: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// generateName mints a unique name from a prefix, retrying on collision.
func (s *Store) generateName(kind, prefix string) (string, error) {
	for i := 0; i < 8; i++ {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", api.NewInternalError("generate name: %v", err)
		}
		name := prefix + hex.EncodeToString(b[:])
		if _, taken := s.objects[kind][name]; !taken {
			return name, nil
		}
	}
	return "", api.NewInternalError("could not generate a free name for %s after 8 tries", kind)
}

// listNames returns the names of a kind, sorted, so listing is deterministic.
func (s *Store) listNames(kind string) []string {
	byName := s.objects[kind]
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }
