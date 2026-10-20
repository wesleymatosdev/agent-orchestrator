package hermes

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestContinuationCapabilitiesReportsProviderAssignedIdentity(t *testing.T) {
	plugin := New()
	caps := plugin.ContinuationCapabilities()
	if caps.FreshNativeSessionID != ports.FreshNativeSessionIDProviderAssigned {
		t.Fatalf("FreshNativeSessionID = %q, want provider-assigned", caps.FreshNativeSessionID)
	}
	var _ ports.AgentContinuationCapabilityProvider = plugin
	var _ ports.AgentNativeSessionConfigProvider = plugin
}

func TestNativeSessionConfigDirPrefersLaunchEnvironment(t *testing.T) {
	plugin := New()
	explicit := filepath.Join(t.TempDir(), "profiles", "switch-target")
	dir, err := plugin.NativeSessionConfigDir(context.Background(), map[string]string{"HERMES_HOME": explicit})
	if err != nil {
		t.Fatalf("NativeSessionConfigDir: %v", err)
	}
	if dir != filepath.Clean(explicit) {
		t.Fatalf("config dir = %q, want %q", dir, explicit)
	}
}

func TestNativeSessionConfigDirFallsBackToHome(t *testing.T) {
	t.Setenv("HERMES_HOME", "")
	home := t.TempDir()
	plugin := New()
	dir, err := plugin.NativeSessionConfigDir(context.Background(), map[string]string{"HOME": home})
	if err != nil {
		t.Fatalf("NativeSessionConfigDir: %v", err)
	}
	if want := filepath.Join(home, ".hermes"); dir != want {
		t.Fatalf("config dir = %q, want %q", dir, want)
	}
}
