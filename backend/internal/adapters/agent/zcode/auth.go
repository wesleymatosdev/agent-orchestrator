package zcode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.AgentAuthChecker = (*Plugin)(nil)

// AuthStatus returns the plugin's local authentication status.
//
// The credential that actually governs model access is zcode's OAuth token in
// ~/.zcode/v2/credentials.json (the apiKey strings in cli/config.json stay
// populated even after the OAuth session expires). When the access token is a
// plaintext JWT, its exp claim is decoded locally — no network call, no
// signature verification; expiry is a sufficient local signal because the
// daemon rejects stale tokens on first real use anyway.
//
// Not every install stores a decodable JWT. Verified against zcode-runtime
// 0.16.5 on Windows, one real credential shape is an opaque three-part token
// (15- and 16-byte first parts, none of which decode as JSON) alongside
// zcode itself reporting "Model access is not configured · Run /login" — so
// token presence is NOT evidence of a working login. Such tokens report
// Unknown; the terminal surfaces zcode's own /login guidance on first use.
func (p *Plugin) AuthStatus(ctx context.Context) (ports.AgentAuthStatus, error) {
	if _, err := p.ResolveBinary(ctx); err != nil {
		return ports.AgentAuthStatusUnknown, err
	}
	return zcodeCredentialsAuthStatus(), nil
}

func zcodeCredentialsAuthStatus() ports.AgentAuthStatus {
	zcodeHome, err := zcodeConfigDir()
	if err != nil || zcodeHome == "" {
		return ports.AgentAuthStatusUnknown
	}
	data, err := os.ReadFile(filepath.Join(zcodeHome, "v2", "credentials.json")) //nolint:gosec // fixed user-credentials path
	if os.IsNotExist(err) {
		return ports.AgentAuthStatusUnauthorized
	}
	if err != nil {
		return ports.AgentAuthStatusUnknown
	}

	var creds struct {
		OAuthZaiAccessToken string `json:"oauth:zai:access_token"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return ports.AgentAuthStatusUnknown
	}
	token := strings.TrimSpace(creds.OAuthZaiAccessToken)
	if token == "" {
		return ports.AgentAuthStatusUnauthorized
	}
	exp, ok := jwtExpiry(token)
	if !ok {
		// Present but not a decodable JWT: an observed real credential
		// shape whose validity cannot be determined locally. Deliberately
		// not Authorized — a verified install carried exactly this shape
		// while zcode itself reported the login as not configured.
		return ports.AgentAuthStatusUnknown
	}
	if time.Now().After(time.Unix(exp, 0)) {
		return ports.AgentAuthStatusUnauthorized
	}
	return ports.AgentAuthStatusAuthorized
}

// jwtExpiry decodes the unverified exp claim of a JWT. It returns ok=false
// for malformed tokens rather than guessing.
func jwtExpiry(token string) (int64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0, false
	}
	return claims.Exp, true
}

func zcodeConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", nil
	}
	return filepath.Join(home, ".zcode"), nil
}
