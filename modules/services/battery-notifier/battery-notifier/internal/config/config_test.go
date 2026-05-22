package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_Valid(t *testing.T) {
	p := writeTemp(t, `{
		"devices": [
			{"name": "laptop", "kind": "laptop",
			 "lowThreshold": 20, "fullThreshold": 100,
			 "dismissThreshold": 50, "ignoreZero": false},
			{"name": "mouse", "kind": "razer",
			 "lowThreshold": 20, "fullThreshold": 100,
			 "dismissThreshold": 50, "ignoreZero": true}
		]
	}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(c.Devices))
	}
	if c.Devices[1].Kind != KindRazer || !c.Devices[1].IgnoreZero {
		t.Errorf("mouse device mis-parsed: %+v", c.Devices[1])
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_DuplicateName(t *testing.T) {
	p := writeTemp(t, `{
		"devices": [
			{"name": "x", "kind": "laptop", "lowThreshold": 20,
			 "fullThreshold": 100, "dismissThreshold": 50},
			{"name": "x", "kind": "razer", "lowThreshold": 20,
			 "fullThreshold": 100, "dismissThreshold": 50}
		]
	}`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected duplicate-name error")
	}
}

func TestLoad_UnknownKind(t *testing.T) {
	p := writeTemp(t, `{"devices": [{"name": "x", "kind": "wat",
		"lowThreshold": 20, "fullThreshold": 100, "dismissThreshold": 50}]}`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected unknown-kind error")
	}
}

func TestLoad_DismissBelowLow(t *testing.T) {
	p := writeTemp(t, `{"devices": [{"name": "x", "kind": "laptop",
		"lowThreshold": 50, "fullThreshold": 100, "dismissThreshold": 30}]}`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected dismiss<low validation error")
	}
}

func TestLoad_OutOfRangeThreshold(t *testing.T) {
	p := writeTemp(t, `{"devices": [{"name": "x", "kind": "laptop",
		"lowThreshold": 120, "fullThreshold": 100, "dismissThreshold": 50}]}`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected out-of-range error")
	}
}
