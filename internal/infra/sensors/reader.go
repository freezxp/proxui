package sensors

import (
	"context"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/telemetry"
)

// Reader gets a node's sensors: run the command, parse what comes back.
//
// It exists so the collector depends on "read this node's sensors" rather than
// on SSH and on `sensors -j` output. Both of those are this layer's business —
// the app layer decides which nodes and how often, and would not compile
// against either (docs/05-system-architecture.md).
type Reader struct{ Runner ports.NodeCommandRunner }

// NewReader builds a reader over a command runner.
func NewReader(runner ports.NodeCommandRunner) *Reader { return &Reader{Runner: runner} }

// Read implements ports.NodeSensorReader.
func (r *Reader) Read(ctx context.Context, target ports.SSHTarget, cred ports.SSHCredential,
	policy ports.HostKeyPolicy) ([]telemetry.Reading, error) {
	out, err := r.Runner.RunCommand(ctx, target, cred, policy, Command)
	if err != nil {
		return nil, err
	}
	return Parse(out)
}
