// Package agentbus holds the harness integrations that `agent-bus install`
// writes out. The program lives in src and cmd/agent-bus.
package agentbus

import "embed"

//go:embed integrations
var Integrations embed.FS
