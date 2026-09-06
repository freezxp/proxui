package deploy

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/domain/deployment"
)

func sample() *deployment.Deployment {
	yes := true
	return &deployment.Deployment{
		ID:   uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		Node: "cx1", AppID: "adguard", AppName: "Adguard",
		Spec: deployment.Spec{
			Hostname: "adguard-1", Cores: 2, MemoryMB: 1024, DiskGB: 4,
			Storage: "local-lvm", Bridge: "vmbr0", Unprivileged: &yes,
		},
	}
}

// The runner is where the pins and the settings actually take effect, so it is
// asserted directly rather than through the deployer.
func TestRunnerPinsBothUpstreamRootsAndPassesSettings(t *testing.T) {
	got := runner(sample())

	for _, want := range []string{
		"export COMMUNITY_SCRIPTS_URL='" + RawURL(ScriptsRepo, ScriptsRef) + "'",
		"export COMMUNITY_SCRIPTS_CORE_URL='" + RawURL(EngineRepo, EngineRef) + "'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the runner does not pin an upstream root:\nwant %s\n%s", want, got)
		}
	}
	// Left unpinned the scripts resolve themselves from a branch, which is the
	// thing vendoring exists to stop.
	if strings.Contains(got, "/main") {
		t.Errorf("the runner points at a branch somewhere:\n%s", got)
	}

	for _, want := range []string{
		// Both spellings: the menu that chooses the mode reads only the
		// lowercase one, and without it the engine draws a whiptail menu,
		// reads EOF, and exits 0 having installed nothing.
		"export MODE=default", "export mode=default", "export UNATTENDED=yes",
		// Without TERM the scripts' own tput calls fail and the install dies
		// one line in; there is no PTY on this seam and nothing else sets it.
		"export TERM=",
		"export var_hostname='adguard-1'", "export var_cpu='2'",
		"export var_ram='1024'", "export var_disk='4'",
		"export var_container_storage='local-lvm'", "export var_brg='vmbr0'",
		"export var_unprivileged='1'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the runner is missing %q:\n%s", want, got)
		}
	}
	// The exit status has to outlive the connection that started the install.
	if !strings.Contains(got, "echo $code >./status") {
		t.Errorf("the runner does not record an exit status:\n%s", got)
	}
}

// A setting nobody chose leaves the script's own default alone. Many of these
// scripts pick different memory and disk per container OS, and overriding with
// a guess would be worse than not overriding.
func TestUnsetSettingsAreNotSent(t *testing.T) {
	got := runner(&deployment.Deployment{ID: uuid.New(), AppID: "adguard"})
	for _, unwanted := range []string{"var_cpu", "var_ram", "var_disk", "var_hostname",
		"var_container_storage", "var_brg", "var_unprivileged"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%s was sent for a deployment that did not choose one:\n%s", unwanted, got)
		}
	}
}

// The launch command carries the vendored script as base64 and nothing else.
// Nothing a request contained is written into a command line — the settings
// travel inside a file, and the payload cannot be acted on by a shell.
func TestLaunchCarriesTheScriptAsData(t *testing.T) {
	rec := sample()
	script, err := Script(rec.AppID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := launchCommand(rec, script)

	encoded := base64.StdEncoding.EncodeToString(script)
	if !strings.Contains(cmd, encoded) {
		t.Error("the vendored script is not in the launch command")
	}
	// The script's own text must not appear unencoded: that would mean it had
	// been placed on a command line where a shell reads it.
	if strings.Contains(cmd, "APP=\"Adguard\"") {
		t.Error("the script was placed on the command line rather than encoded")
	}
	if !strings.Contains(cmd, "setsid nohup bash ./run.sh") {
		t.Error("the install is not detached, so its log dies with the connection")
	}
	if !strings.Contains(cmd, workRoot+"/"+rec.ID.String()) {
		t.Error("the work directory is not scoped to this deployment")
	}
	// One command: a connection that dropped between three of them would leave
	// a half-written directory and a deploy that never starts.
	if strings.Count(cmd, "base64 -d") != 2 {
		t.Errorf("expected both files written in one command:\n%s", cmd)
	}
}

func TestPollParsesStatusContainerAndLog(t *testing.T) {
	exit, ctid, log := parsePoll("exit 0\nctid 142\n" + pollMarker + "\nhello\nworld\n")
	if exit == nil || *exit != 0 {
		t.Errorf("exit = %v, want 0", exit)
	}
	if ctid != "142" {
		t.Errorf("ctid = %q, want 142", ctid)
	}
	if log != "hello\nworld\n" {
		t.Errorf("log = %q", log)
	}

	// Still going: no status yet, and the transcript so far.
	exit, _, log = parsePoll("running\nctid \n" + pollMarker + "\npartial\n")
	if exit != nil {
		t.Errorf("exit = %v, want nil while it runs", exit)
	}
	if log != "partial\n" {
		t.Errorf("log = %q", log)
	}

	// A work directory the node no longer has reads as still running rather
	// than as an answer: there is nothing to conclude from it.
	if exit, _, _ = parsePoll("missing\n"); exit != nil {
		t.Errorf("a missing directory produced an exit status: %v", exit)
	}
}

// The transcript is capped, and the end is the part that explains a failure.
func TestLogKeepsTheEnd(t *testing.T) {
	long := strings.Repeat("noise\n", 100000) + "the actual error\n"
	got := deployment.TruncateLog(long)
	if len(got) > deployment.MaxLogBytes+64 {
		t.Errorf("kept %d bytes, want about %d", len(got), deployment.MaxLogBytes)
	}
	if !strings.Contains(got, "the actual error") {
		t.Error("truncation dropped the end, which is where the failure is")
	}
	if !strings.HasPrefix(got, "… earlier output dropped …") {
		t.Error("a truncated log does not say that it was truncated")
	}
}
