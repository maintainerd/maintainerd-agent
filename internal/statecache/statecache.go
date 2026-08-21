// Package statecache persists the agent's system-tier work items to disk so
// they survive a control-plane outage. This is the availability half of the
// platform's static tier: system services (auth, secret, core itself) must
// never die just because the thing that schedules them is unreachable — the
// agent keeps converging them from this cache until Core comes back.
//
// Only tier=="system" items are cached; ordinary workloads correctly pause
// when the control plane is away, because acting on stale desired state for
// user workloads trades correctness for nothing.
package statecache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// fileName is the single cache file under the state dir.
const fileName = "system-workloads.json"

// Item is one cached work item, keyed by its resource UUID. SpecJSON is kept
// verbatim (the envelope the agent pulled) so offline reconciles run the exact
// spec Core last handed out, at the generation it was observed.
type Item struct {
	ResourceUUID string `json:"resource_uuid"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	SpecJSON     string `json:"spec_json"`
	Generation   int64  `json:"generation"`
}

// Cache reads and writes the on-disk store. Safe for use from a single
// goroutine (the worker's pull loop is the only writer).
type Cache struct {
	dir string
}

// New returns a cache rooted at dir. The directory is created lazily on the
// first Save with mode 0700 — the cache can contain workload env/config, so
// it is owner-only, like ~/.ssh.
func New(dir string) *Cache { return &Cache{dir: dir} }

// Path returns the cache file location (for logs and doctor output).
func (c *Cache) Path() string { return filepath.Join(c.dir, fileName) }

// Save atomically replaces the cache with the given items, keyed by resource
// UUID (last write for a UUID wins). The write is tmp-file + rename so a
// crash mid-write can never leave a torn file: offline supervision reading a
// half-written cache would converge garbage, so the file is either the old
// complete state or the new complete state, nothing in between. File mode is
// 0600, directory 0700 — specs may embed configuration secrets.
func (c *Cache) Save(items []Item) error {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return fmt.Errorf("statecache: create dir: %w", err)
	}
	keyed := make(map[string]Item, len(items))
	for _, it := range items {
		if it.ResourceUUID == "" {
			return fmt.Errorf("statecache: item %q has no resource UUID", it.Name)
		}
		keyed[it.ResourceUUID] = it
	}
	blob, err := json.MarshalIndent(keyed, "", "  ")
	if err != nil {
		return fmt.Errorf("statecache: marshal: %w", err)
	}

	tmp, err := os.CreateTemp(c.dir, fileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("statecache: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after successful rename

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("statecache: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(blob); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("statecache: write temp file: %w", err)
	}
	// Sync before rename: the rename is only atomic against a torn FILE, not
	// against data still sitting in the page cache when the host loses power.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("statecache: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("statecache: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, c.Path()); err != nil {
		return fmt.Errorf("statecache: rename into place: %w", err)
	}
	return nil
}

// Load returns the cached items sorted by resource UUID (deterministic
// offline reconcile order). A missing file is an empty cache, not an error —
// a fresh host has simply never seen system work yet. A corrupt file IS an
// error: silently converging from garbage would be worse than reporting it.
func (c *Cache) Load() ([]Item, error) {
	blob, err := os.ReadFile(c.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("statecache: read: %w", err)
	}
	var keyed map[string]Item
	if err := json.Unmarshal(blob, &keyed); err != nil {
		return nil, fmt.Errorf("statecache: parse %s: %w", c.Path(), err)
	}
	out := make([]Item, 0, len(keyed))
	for _, it := range keyed {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceUUID < out[j].ResourceUUID })
	return out, nil
}
