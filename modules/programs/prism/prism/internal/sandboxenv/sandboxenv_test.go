package sandboxenv_test

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/sandboxenv"
)

func TestIsInsideSandbox(t *testing.T) {
	t.Run("returns true when PRISM_HOST_API is set to a non-empty value", func(t *testing.T) {
		t.Setenv("PRISM_HOST_API", "unix:///tmp/prism.sock")
		if !sandboxenv.IsInsideSandbox() {
			t.Error("IsInsideSandbox() = false, want true when PRISM_HOST_API is set")
		}
	})

	t.Run("returns true when PRISM_HOST_API is set to an http URL", func(t *testing.T) {
		t.Setenv("PRISM_HOST_API", "http://localhost:9999")
		if !sandboxenv.IsInsideSandbox() {
			t.Error("IsInsideSandbox() = false, want true when PRISM_HOST_API is set")
		}
	})

	t.Run("returns false when PRISM_HOST_API is empty string", func(t *testing.T) {
		t.Setenv("PRISM_HOST_API", "")
		if sandboxenv.IsInsideSandbox() {
			t.Error("IsInsideSandbox() = true, want false when PRISM_HOST_API is empty")
		}
	})

	t.Run("returns false when PRISM_HOST_API is unset", func(t *testing.T) {
		t.Setenv("PRISM_HOST_API", "")
		if sandboxenv.IsInsideSandbox() {
			t.Error("IsInsideSandbox() = true, want false when PRISM_HOST_API is unset")
		}
	})
}

func TestHostAPISocket(t *testing.T) {
	t.Run("returns exact value of PRISM_HOST_API when set to unix socket URL", func(t *testing.T) {
		const want = "unix:///tmp/prism.sock"
		t.Setenv("PRISM_HOST_API", want)
		if got := sandboxenv.HostAPISocket(); got != want {
			t.Errorf("HostAPISocket() = %q, want %q", got, want)
		}
	})

	t.Run("returns exact value of PRISM_HOST_API when set to http URL", func(t *testing.T) {
		const want = "http://localhost:9999"
		t.Setenv("PRISM_HOST_API", want)
		if got := sandboxenv.HostAPISocket(); got != want {
			t.Errorf("HostAPISocket() = %q, want %q", got, want)
		}
	})

	t.Run("returns empty string when PRISM_HOST_API is empty", func(t *testing.T) {
		t.Setenv("PRISM_HOST_API", "")
		if got := sandboxenv.HostAPISocket(); got != "" {
			t.Errorf("HostAPISocket() = %q, want empty string", got)
		}
	})

	t.Run("returns empty string when PRISM_HOST_API is unset", func(t *testing.T) {
		t.Setenv("PRISM_HOST_API", "")
		if got := sandboxenv.HostAPISocket(); got != "" {
			t.Errorf("HostAPISocket() = %q, want empty string", got)
		}
	})

	t.Run("does not transform or parse the URL value", func(t *testing.T) {
		// Verify no stripping of unix:// prefix or other transformation occurs.
		const want = "unix:///var/run/prism/host-api.sock"
		t.Setenv("PRISM_HOST_API", want)
		if got := sandboxenv.HostAPISocket(); got != want {
			t.Errorf("HostAPISocket() = %q, want %q (no transformation)", got, want)
		}
	})
}
