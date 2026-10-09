package database

import (
	"fmt"
	"sync"
	"testing"

	"github.com/cryskram/veridb/internal/config"
)

func overrideTestEntry(name string) *Entry {
	return &Entry{
		Config: config.ResolvedDatabase{
			Name:     name,
			Database: name + "_db",
			Policy:   config.Policy{ReadOnly: true, AllowWrite: false, AllowDelete: false},
			Services: config.Services{Query: true, Admin: true},
		},
	}
}

func TestSetWriteModeUnknownDatabase(t *testing.T) {
	r := NewRegistry()

	if err := r.SetWriteMode("nope", true); err == nil {
		t.Error("expected an error for an unknown database")
	}
	if r.WriteMode("nope") {
		t.Error("unknown database must report no override")
	}
	if _, err := r.EffectiveConfig("nope"); err == nil {
		t.Error("expected an error from EffectiveConfig for an unknown database")
	}
}

func TestWriteModeRoundTrip(t *testing.T) {
	r := NewRegistry()
	if err := r.Add(overrideTestEntry("app")); err != nil {
		t.Fatal(err)
	}

	if r.WriteMode("app") {
		t.Error("override must start off")
	}

	if err := r.SetWriteMode("app", true); err != nil {
		t.Fatal(err)
	}
	if !r.WriteMode("app") {
		t.Error("override should be active after enabling")
	}

	eff, err := r.EffectiveConfig("app")
	if err != nil {
		t.Fatal(err)
	}
	if eff.Policy.ReadOnly {
		t.Error("effective policy should be writable")
	}
	if !eff.Policy.AllowWrite {
		t.Error("effective policy should allow writes")
	}
	if eff.Policy.AllowDelete {
		t.Error("override must never open DELETE")
	}
	if !eff.Services.Write {
		t.Error("effective services should expose write")
	}

	// The base config must be untouched: a restart drops the override only
	// because there is nothing else to drop.
	base, err := r.Get("app")
	if err != nil {
		t.Fatal(err)
	}
	if !base.Config.Policy.ReadOnly {
		t.Error("base policy must stay read-only")
	}
	if base.Config.Services.Write {
		t.Error("base services must stay write-off")
	}

	if err := r.SetWriteMode("app", false); err != nil {
		t.Fatal(err)
	}
	if r.WriteMode("app") {
		t.Error("override should be cleared after disabling")
	}
	eff, err = r.EffectiveConfig("app")
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Policy.ReadOnly || eff.Services.Write {
		t.Error("effective config should equal the base config again")
	}
}

// The override store is read on every tool call, so it must be safe under
// concurrent toggling and reading. Run with -race.
func TestWriteModeConcurrent(t *testing.T) {
	r := NewRegistry()
	if err := r.Add(overrideTestEntry("app")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = r.SetWriteMode("app", (i+j)%2 == 0)
				_ = r.WriteMode("app")
				_, _ = r.EffectiveConfig("app")
			}
		}(i)
	}
	wg.Wait()

	if err := r.SetWriteMode("app", false); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(r.WriteMode("app")); got != "false" {
		t.Errorf("final state = %s, want false", got)
	}
}
