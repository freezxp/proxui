package deploy

import (
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/domain/deployment"
)

// The commands that run on a node (ADR 0012).
//
// Everything here is built from three sources and no others: constants in this
// file, the vendored script bytes, and settings the domain has already
// validated as numbers or as identifiers. Nothing a request carried is written
// into a command; the settings that vary reach the node inside a file the
// portal wrote, base64-encoded so that no quoting decision is ever made about
// them on a command line.

const (
	// launchedMarker is what the node says when the script is running. A
	// specific word rather than an exit status, because `setsid … &` succeeds
	// whether or not the thing behind it did.
	launchedMarker = "proxui-deploy-started"
	// pollMarker separates the status line from the transcript, so one command
	// answers both questions and the log cannot be mistaken for the status.
	pollMarker = "---proxui-log---"
	// logTailBytes is how much of the transcript comes back each poll. The
	// runner keeps the whole thing on the node; this is what the portal stores
	// and shows, and it is the end that explains a failure.
	logTailBytes = 200 << 10
)

// workDir is where one deployment's files live on the node. The identifier is a
// UUID the portal generated, so the path is not built from anything a caller
// chose.
func workDir(id uuid.UUID) string { return workRoot + "/" + id.String() }

// runner is the script the portal writes beside the vendored one.
//
// It exists so that the settings become `export` lines in a file rather than
// arguments on a command line, and so that the exit status is recorded after
// the install rather than lost with the connection that started it. It also
// notes which container appeared, because the script allocates the id and never
// says so in a form worth parsing.
func runner(rec *deployment.Deployment) string {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString("cd " + workDir(rec.ID) + " || exit 127\n")

	// Unattended three ways over: the engine honours MODE and UNATTENDED, and
	// treats a stdin that is not a terminal as unattended too — which this seam
	// gives it anyway. Belt and braces, because a script that stops at a
	// whiptail prompt would hang until the deploy window closed.
	// Both spellings, and they are not redundant. is_unattended() reads either
	// MODE or mode; the menu that *chooses* the mode reads only the lowercase
	// one, so setting MODE alone leaves the engine drawing a whiptail options
	// menu, reading EOF from a stdin that is /dev/null, and reporting that the
	// user exited. Found by deploying against a live node — with an exit status
	// of 0, so it looked like a success that installed nothing.
	b.WriteString("export MODE=default\n")
	b.WriteString("export mode=default\n")
	b.WriteString("export UNATTENDED=yes\n")
	b.WriteString("export PHS_SILENT=1\n")
	b.WriteString("export DEBIAN_FRONTEND=noninteractive\n")
	// The scripts colour their output with tput, which exits non-zero with
	// "TERM environment variable not set." when there is no terminal — and this
	// seam deliberately opens no PTY. Found by deploying against a live node:
	// the install died one line in, before anything had been created.
	b.WriteString("export TERM=xterm-256color\n")

	// Both upstream roots, pinned. Left unset, the scripts resolve themselves
	// from a branch, and the deploy would run whatever was pushed that morning.
	b.WriteString("export COMMUNITY_SCRIPTS_URL=" + quote(RawURL(ScriptsRepo, ScriptsRef)) + "\n")
	b.WriteString("export COMMUNITY_SCRIPTS_CORE_URL=" + quote(RawURL(EngineRepo, EngineRef)) + "\n")

	for _, v := range settings(rec.Spec) {
		b.WriteString("export " + v.key + "=" + quote(v.value) + "\n")
	}

	// Which containers existed before, so the one that appears can be named.
	// Informational: the inventory is what actually surfaces the container, and
	// two deploys racing on one node could attribute it to the wrong record.
	b.WriteString("before=$(pct list 2>/dev/null | awk 'NR>1{print $1}' | sort)\n")
	b.WriteString("bash ./app.sh >./log 2>&1\n")
	b.WriteString("code=$?\n")
	b.WriteString("after=$(pct list 2>/dev/null | awk 'NR>1{print $1}' | sort)\n")
	b.WriteString("comm -13 <(echo \"$before\") <(echo \"$after\") | head -1 >./ctid\n")
	b.WriteString("echo $code >./status\n")
	return b.String()
}

type setting struct{ key, value string }

// settings turns what the operator chose into the environment the scripts read.
//
// Only what was actually chosen: an unset field leaves the script's own default
// alone, which matters because many of these branch on the container OS and
// pick different memory and disk for each.
func settings(spec deployment.Spec) []setting {
	var out []setting
	add := func(key, value string) {
		if value != "" {
			out = append(out, setting{key, value})
		}
	}
	add("var_hostname", spec.Hostname)
	add("var_cpu", number(spec.Cores))
	add("var_ram", number(spec.MemoryMB))
	add("var_disk", number(spec.DiskGB))
	add("var_container_storage", spec.Storage)
	add("var_brg", spec.Bridge)
	if spec.Unprivileged != nil {
		value := "0"
		if *spec.Unprivileged {
			value = "1"
		}
		add("var_unprivileged", value)
	}
	return out
}

func number(v int) string {
	if v <= 0 {
		return ""
	}
	return strconv.Itoa(v)
}

// quote wraps a value for the shell. The domain has already refused anything
// with a quote in it, so this cannot be escaped out of; it is here because
// relying on a value's shape without saying so is how the next person removes
// the validation and nothing appears to break.
func quote(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

// launchCommand writes both scripts onto the node and starts them detached.
//
// One command rather than three: RunCommand opens a connection per command, and
// a half-written work directory left behind by a connection that dropped
// between them would be a deploy that never starts and never says why.
func launchCommand(rec *deployment.Deployment, script []byte) string {
	dir := workDir(rec.ID)
	b64 := base64.StdEncoding.EncodeToString
	return strings.Join([]string{
		"set -e",
		"mkdir -p " + dir,
		"cd " + dir,
		// printf rather than echo: the payload is base64, which cannot contain
		// anything a shell would act on, and printf %s makes that explicit.
		"printf %s " + quote(b64(script)) + " | base64 -d >app.sh",
		"printf %s " + quote(b64([]byte(runner(rec)))) + " | base64 -d >run.sh",
		"chmod 700 app.sh run.sh",
		// Detached from this connection: the install outlives it by design.
		"setsid nohup bash ./run.sh </dev/null >/dev/null 2>&1 &",
		"echo " + launchedMarker,
	}, "\n")
}

// pollCommand asks how far the script has got, in one exchange: the exit status
// if it has one, the container it made if it has made one, and the end of the
// transcript.
func pollCommand(id uuid.UUID) string {
	dir := workDir(id)
	return strings.Join([]string{
		"cd " + dir + " 2>/dev/null || { echo missing; exit 0; }",
		`if [ -f status ]; then echo "exit $(cat status)"; else echo running; fi`,
		`echo "ctid $(cat ctid 2>/dev/null | tr -dc '0-9')"`,
		"echo " + pollMarker,
		"tail -c " + strconv.Itoa(logTailBytes) + " log 2>/dev/null || true",
	}, "\n")
}

// parsePoll reads what pollCommand answered: the exit status if the script has
// finished, the container id if it is known, and the transcript.
//
// A missing work directory is treated as still running rather than as an error.
// It means the node was rebooted or something swept /var/tmp, and there is
// nothing useful to conclude from it beyond waiting for the window to close.
func parsePoll(out string) (exit *int, ctid string, log string) {
	head, tail, found := strings.Cut(out, pollMarker+"\n")
	if !found {
		head, tail, _ = strings.Cut(out, pollMarker)
	}
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "exit "):
			if code, ok := atoi(strings.TrimPrefix(line, "exit ")); ok {
				exit = &code
			}
		case strings.HasPrefix(line, "ctid "):
			ctid = strings.TrimSpace(strings.TrimPrefix(line, "ctid "))
		}
	}
	return exit, ctid, deployment.TruncateLog(tail)
}
