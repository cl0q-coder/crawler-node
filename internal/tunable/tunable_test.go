package tunable

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// IG6: tunables must update atomically and enforce politeness floors.

func TestUpdate(t *testing.T) {
	tun := New(time.Second, time.Second, 50)

	// Update host delay.
	changed := tun.Update(Config{HostDelayMs: intPtr(2000)})
	if !changed {
		t.Error("Update should report changed=true")
	}
	if d := tun.HostDelay(); d != 2*time.Second {
		t.Errorf("HostDelay = %v, want 2s", d)
	}

	// No change if same value.
	changed = tun.Update(Config{HostDelayMs: intPtr(2000)})
	if changed {
		t.Error("Update with same value should report changed=false")
	}

	// Politeness floor: 1s minimum enforced.
	tun.Update(Config{HostDelayMs: intPtr(100)})
	if d := tun.HostDelay(); d != time.Second {
		t.Errorf("HostDelay below floor: got %v, want 1s (floor enforced)", d)
	}

	// Partial update: only specified fields change.
	tun.Update(Config{MaxConcurrency: intPtr(100)})
	if c := tun.MaxConcurrency(); c != 100 {
		t.Errorf("MaxConcurrency = %d, want 100", c)
	}
	if d := tun.IPDelay(); d != time.Second {
		t.Errorf("IPDelay should be unchanged, got %v", d)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.json")
	content := `{"host_delay_ms": 1500, "max_concurrency": 75}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile error: %v", err)
	}
	if c.HostDelayMs == nil || *c.HostDelayMs != 1500 {
		t.Errorf("HostDelayMs = %v, want 1500", c.HostDelayMs)
	}
	if c.IPDelayMs != nil {
		t.Error("IPDelayMs should be nil (not in file)")
	}
	if c.MaxConcurrency == nil || *c.MaxConcurrency != 75 {
		t.Errorf("MaxConcurrency = %v, want 75", c.MaxConcurrency)
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := LoadFile("/nonexistent/control.json")
	if err == nil {
		t.Error("LoadFile should error on missing file")
	}
}

func intPtr(i int) *int { return &i }
