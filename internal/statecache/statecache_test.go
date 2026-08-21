package statecache

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func item(uuid string, gen int64) Item {
	return Item{ResourceUUID: uuid, Kind: "container", Name: "n-" + uuid, SpecJSON: `{"workload":{}}`, Generation: gen}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	c := New(t.TempDir())
	in := []Item{item("b", 2), item("a", 1)}

	if err := c.Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := c.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Load returned %d items, want 2", len(got))
	}
	// Deterministic order: sorted by resource UUID.
	if got[0].ResourceUUID != "a" || got[1].ResourceUUID != "b" {
		t.Errorf("order = %s,%s, want a,b", got[0].ResourceUUID, got[1].ResourceUUID)
	}
	if got[0].Generation != 1 || got[1].Generation != 2 {
		t.Errorf("generations = %d,%d, want 1,2", got[0].Generation, got[1].Generation)
	}
	if got[0].SpecJSON != `{"workload":{}}` {
		t.Errorf("spec JSON not preserved verbatim: %q", got[0].SpecJSON)
	}
}

func TestSaveKeysByUUIDLastWins(t *testing.T) {
	c := New(t.TempDir())
	if err := c.Save([]Item{item("a", 1), item("a", 5)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := c.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Generation != 5 {
		t.Fatalf("got %+v, want single item at generation 5", got)
	}
}

func TestSaveIsAtomicNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	c := New(dir)

	if err := c.Save([]Item{item("a", 1)}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := c.Save([]Item{item("a", 2), item("b", 1)}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	// tmp+rename must leave exactly the final file — a lingering tmp file
	// means a torn write path exists.
	if len(entries) != 1 || entries[0].Name() != fileName {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir contents = %v, want only %s", names, fileName)
	}
	got, err := c.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Load returned %d items, want 2", len(got))
	}
}

func TestSavePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "state") // created by Save itself
	c := New(dir)

	if err := c.Save([]Item{item("a", 1)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 700 (specs can hold secrets)", perm)
	}
	fileInfo, err := os.Stat(c.Path())
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600 (specs can hold secrets)", perm)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "never-created"))
	got, err := c.Load()
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d items, want 0", len(got))
	}
}

func TestLoadCorruptFileFailsLoud(t *testing.T) {
	dir := t.TempDir()
	c := New(dir)
	if err := os.WriteFile(c.Path(), []byte("{torn"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	if _, err := c.Load(); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("Load = %v, want parse error — converging from garbage must not be silent", err)
	}
}

func TestSaveRejectsItemWithoutUUID(t *testing.T) {
	c := New(t.TempDir())
	if err := c.Save([]Item{{Name: "orphan"}}); err == nil {
		t.Fatal("Save accepted an item without a resource UUID")
	}
}
