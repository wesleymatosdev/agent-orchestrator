package zcode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func writeZcodeCredentials(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".zcode", "v2")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir credentials dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
}

// testJWT builds a minimal unsigned JWT with the given exp claim.
func testJWT(t *testing.T, exp int64) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	claims, err := json.Marshal(map[string]any{"exp": exp})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(header) + "." + enc.EncodeToString(claims) + ".sig"
}

func TestAuthStatusAuthorizedWithUnexpiredToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeZcodeCredentials(t, home, `{"oauth:zai:access_token": "`+testJWT(t, time.Now().Add(time.Hour).Unix())+`"}`)
	plugin := &Plugin{resolvedBinary: "zcode"}
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if status != ports.AgentAuthStatusAuthorized {
		t.Fatalf("status = %q, want authorized", status)
	}
}

func TestAuthStatusUnauthorizedWithExpiredToken(t *testing.T) {
	// The live incident: OAuth expired while apiKey strings stayed in
	// cli/config.json. The checker must read the real credential's exp.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeZcodeCredentials(t, home, `{"oauth:zai:access_token": "`+testJWT(t, time.Now().Add(-time.Hour).Unix())+`"}`)
	plugin := &Plugin{resolvedBinary: "zcode"}
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if status != ports.AgentAuthStatusUnauthorized {
		t.Fatalf("status = %q, want unauthorized", status)
	}
}

func TestAuthStatusUnauthorizedWithoutCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	plugin := &Plugin{resolvedBinary: "zcode"}
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if status != ports.AgentAuthStatusUnauthorized {
		t.Fatalf("status = %q, want unauthorized", status)
	}
}

func TestAuthStatusUnauthorizedWithEmptyToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeZcodeCredentials(t, home, `{"oauth:zai:access_token": ""}`)
	plugin := &Plugin{resolvedBinary: "zcode"}
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if status != ports.AgentAuthStatusUnauthorized {
		t.Fatalf("status = %q, want unauthorized", status)
	}
}

// opaqueToken reproduces the credential shape observed on a real install
// (zcode-runtime 0.16.5, Windows): three dot-separated parts of undecodable
// binary — 15- and 16-byte first parts, ~1.4 KB third part — none of which
// decode to JSON. jwtExpiry must refuse it, and its presence must not be
// mistaken for a working login: that install showed zcode's own "Model access
// is not configured · Run /login" banner.
func opaqueToken() string {
	return base64.RawURLEncoding.EncodeToString(make([]byte, 15)) + "." +
		base64.RawURLEncoding.EncodeToString(make([]byte, 16)) + "." +
		base64.RawURLEncoding.EncodeToString(make([]byte, 1404))
}

func TestAuthStatusUnknownWithOpaqueToken(t *testing.T) {
	// Non-JWT tokens carry no locally decodable expiry, and a verified
	// install in exactly this state was reported by zcode itself as not
	// configured — so this is Unknown, not Authorized.
	for name, token := range map[string]string{
		"opaque-three-part": opaqueToken(),
		"single-part":       "not-a-jwt",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeZcodeCredentials(t, home, `{"oauth:zai:access_token": "`+token+`"}`)
			plugin := &Plugin{resolvedBinary: "zcode"}
			status, err := plugin.AuthStatus(context.Background())
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if status != ports.AgentAuthStatusUnknown {
				t.Fatalf("status = %q, want unknown (undecodable token is not evidence of a working login)", status)
			}
		})
	}
}

func TestAuthStatusUnknownWhenCredentialsUnreadable(t *testing.T) {
	// Unknown is reserved for inspection failures, not credential states.
	// A directory in place of the file forces a read error on every OS.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".zcode", "v2", "credentials.json"), 0o750); err != nil {
		t.Fatalf("mkdir credentials-as-dir: %v", err)
	}
	plugin := &Plugin{resolvedBinary: "zcode"}
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if status != ports.AgentAuthStatusUnknown {
		t.Fatalf("status = %q, want unknown (read failure is not evidence either way)", status)
	}
}

func TestAuthStatusUnknownWithMalformedCredentialsJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeZcodeCredentials(t, home, `{not json`)
	plugin := &Plugin{resolvedBinary: "zcode"}
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if status != ports.AgentAuthStatusUnknown {
		t.Fatalf("status = %q, want unknown (malformed credential store is not evidence either way)", status)
	}
}
