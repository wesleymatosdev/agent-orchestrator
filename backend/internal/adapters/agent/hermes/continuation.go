package hermes

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/nativeconfig"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var (
	_ ports.AgentContinuationCapabilityProvider = (*Plugin)(nil)
	_ ports.AgentNativeSessionConfigProvider    = (*Plugin)(nil)
)

// ContinuationCapabilities reports that Hermes assigns its own native session
// ids and AO learns them through the adapter's proven identity chain (the
// terminal breadcrumb plus state.db proof in GetRestoreCommand, and
// NativeConversationID for the Chat surface). Hermes' CLI accepts no
// caller-assigned session id on creation, so provider-assigned is the only
// honest fresh-identity mode. AO activates Hermes as a switch target only
// through its Chat driver; the native terminal emits no AO hook surface, so a
// terminal target could neither stage its fresh native id before activation
// nor acknowledge continuation delivery.
func (p *Plugin) ContinuationCapabilities() ports.ContinuationCapabilities {
	return ports.ContinuationCapabilities{
		FreshNativeSessionID: ports.FreshNativeSessionIDProviderAssigned,
	}
}

// NativeSessionConfigDir returns the exact HERMES_HOME the launch uses, falling
// back to Hermes' standard ~/.hermes state root. The switch saga stores this
// path with each native-session binding so a later resume probe never inspects
// another account or profile's conversations.
func (p *Plugin) NativeSessionConfigDir(ctx context.Context, env map[string]string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir, err := nativeconfig.Resolve(env, "HERMES_HOME", ".hermes")
	if err != nil {
		return "", fmt.Errorf("hermes: resolve config dir: %w", err)
	}
	return dir, nil
}
