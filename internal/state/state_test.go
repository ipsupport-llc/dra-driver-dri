package state_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ipsupport-llc/dra-driver-dri/internal/state"
)

func dev(name string) state.Device {
	return state.Device{Request: "gpu", Pool: "node-a", Device: name, VRAMBytes: 1 << 30, CDIDeviceIDs: []string{"k8s.x/gpu=" + name}}
}

func TestOpenMissing(t *testing.T) {
	s, err := state.Open(filepath.Join(t.TempDir(), "checkpoint.json"))
	if err != nil || len(s.All()) != 0 {
		t.Fatalf("got %v, %v", s.All(), err)
	}
}

func TestPutGetPersistDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("uid-1", []state.Device{dev("a")}); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Get("uid-1"); !ok || len(got) != 1 || got[0].Device != "a" {
		t.Fatalf("Get = %v, %v", got, ok)
	}
	reopened, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Get("uid-1"); !ok || got[0].CDIDeviceIDs[0] != "k8s.x/gpu=a" {
		t.Fatalf("after reopen: %v, %v", got, ok)
	}
	if err := reopened.Delete("uid-1"); err != nil {
		t.Fatal(err)
	}
	again, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Get("uid-1"); ok {
		t.Fatal("claim survived Delete")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestAllIsACopy(t *testing.T) {
	s, err := state.Open(filepath.Join(t.TempDir(), "c.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("uid-1", []state.Device{dev("a")}); err != nil {
		t.Fatal(err)
	}
	all := s.All()
	all["uid-1"][0].Device = "mutated"
	if got, _ := s.Get("uid-1"); got[0].Device != "a" {
		t.Fatal("All returned shared memory")
	}
}

func TestOpenCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Open(path); err == nil {
		t.Fatal("want an error for a corrupt checkpoint")
	}
}

func TestOpenWrongVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte(`{"version": 99, "claims": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Open(path); err == nil {
		t.Fatal("want an error for an unknown version")
	}
}
