package sensors

import (
	"context"
	"errors"
	"testing"

	"github.com/freezxp/proxui/internal/app/ports"
)

type recordingRunner struct {
	commands []string
	out      string
	err      error
}

func (r *recordingRunner) RunCommand(_ context.Context, _ ports.SSHTarget, _ ports.SSHCredential,
	_ ports.HostKeyPolicy, command string) ([]byte, error) {
	r.commands = append(r.commands, command)
	return []byte(r.out), r.err
}

// The command is a constant. If this test has to change because something
// wants to pass a different one, that is the moment ADR 0007's boundary moved:
// a node is reachable for this command and nothing else.
func TestReaderRunsOnlyTheFixedCommand(t *testing.T) {
	runner := &recordingRunner{out: `{"c":{"Package id 0":{"temp1_input":47.0}}}`}
	readings, err := (&Reader{Runner: runner}).Read(context.Background(),
		ports.SSHTarget{Host: "10.0.30.111", Port: 22},
		ports.SSHCredential{Username: "root", PrivateKey: "key"}, nopPolicy{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(runner.commands) != 1 || runner.commands[0] != "sensors -j" {
		t.Fatalf("ran %v, want exactly [sensors -j]", runner.commands)
	}
	if len(readings) != 1 || readings[0].Value != 47 {
		t.Errorf("readings = %+v, want the 47°C package", readings)
	}
}

func TestReaderPassesTheConnectionFailureThrough(t *testing.T) {
	wanted := errors.New("refused")
	_, err := (&Reader{Runner: &recordingRunner{err: wanted}}).Read(context.Background(),
		ports.SSHTarget{Host: "10.0.30.111", Port: 22}, ports.SSHCredential{}, nopPolicy{})
	if !errors.Is(err, wanted) {
		t.Errorf("got %v, want the connection's own error", err)
	}
}

type nopPolicy struct{}

func (nopPolicy) Check(string, string, string, []byte) error { return nil }
