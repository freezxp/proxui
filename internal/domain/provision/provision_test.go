package provision

// The ordering rules are the substance of provisioning, so they are tested
// here where no database or hypervisor is involved: configure before start, or
// cloud-init has nothing to read; grow the disk before boot, or the filesystem
// inside never matches it.

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func provisionRequest(spec Spec) *Request {
	return &Request{
		ID: uuid.New(), PlatformID: uuid.New(), Kind: KindProvision,
		State: StatePending, TemplateExternalID: "9000", GuestName: "web-02",
		Spec: spec,
	}
}

// walk runs a request to a terminal state and reports the path it took.
func walk(t *testing.T, r *Request) []State {
	t.Helper()
	seen := []State{r.State}
	for i := 0; !r.State.Terminal(); i++ {
		if i > 10 {
			t.Fatalf("state machine did not terminate: %v", seen)
		}
		if err := r.Advance(time.Now()); err != nil {
			t.Fatalf("advance: %v", err)
		}
		seen = append(seen, r.State)
	}
	return seen
}

func TestFullProvisioningRunVisitsEveryStepInOrder(t *testing.T) {
	r := provisionRequest(Spec{DiskGrowBytes: 20 << 30, DiskName: "scsi0", StartAfterCreate: true})

	// Verifying sits between starting and ready because "the platform accepted
	// every call" and "the machine came up" are different claims, and only the
	// second is what `ready` was being read as (PROV-16).
	want := []State{StatePending, StateCloning, StateConfiguring, StateResizing,
		StateStarting, StateVerifying, StateReady}
	got := walk(t, r)

	if len(got) != len(want) {
		t.Fatalf("path = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("path = %v, want %v", got, want)
		}
	}
}

// Sending a no-op resize is not harmless: the platform rejects a growth of
// zero, so a request that asked for no extra disk must not enter the step.
func TestResizeIsSkippedWhenNoGrowthWasAsked(t *testing.T) {
	r := provisionRequest(Spec{StartAfterCreate: true})

	for _, s := range walk(t, r) {
		if s == StateResizing {
			t.Fatal("a request with no disk growth entered resizing")
		}
	}
}

func TestStartIsSkippedWhenTheGuestShouldStayOff(t *testing.T) {
	r := provisionRequest(Spec{DiskGrowBytes: 1 << 30, DiskName: "scsi0"})

	got := walk(t, r)
	for _, s := range got {
		if s == StateStarting {
			t.Fatalf("a request that asked for no start entered starting: %v", got)
		}
	}
	if got[len(got)-1] != StateReady {
		t.Errorf("finished at %s, want ready", got[len(got)-1])
	}
}

// Both conditional steps skipped at once is the shortest legal path, and the
// fallthrough that implements it is the easiest thing here to get wrong.
func TestShortestRunIsCloneConfigureReady(t *testing.T) {
	got := walk(t, provisionRequest(Spec{}))

	want := []State{StatePending, StateCloning, StateConfiguring, StateReady}
	if len(got) != len(want) {
		t.Fatalf("path = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("path = %v, want %v", got, want)
		}
	}
}

func TestDestroyHasItsOwnPath(t *testing.T) {
	r := &Request{
		ID: uuid.New(), PlatformID: uuid.New(), Kind: KindDestroy,
		State: StatePending, VMID: "135",
	}
	got := walk(t, r)

	want := []State{StatePending, StateDeleting, StateDeleted}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("path = %v, want %v", got, want)
		}
	}
}

// A task handle belongs to the step that started it. Carrying it into the next
// step would have that step poll the previous task, see it finished, and
// conclude its own work was done.
func TestAdvanceClearsTheTaskHandle(t *testing.T) {
	r := provisionRequest(Spec{})
	r.TaskID = "UPID:pve:0000:clone::root@pam:"

	if err := r.Advance(time.Now()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if r.TaskID != "" {
		t.Errorf("task handle survived the step that owned it: %q", r.TaskID)
	}
}

// A failed run leaves the guest where it is. Destroying on failure would mean
// removing a machine on the strength of an error that may have been misread —
// a clone frequently reports failure over a machine that came up fine.
func TestFailureKeepsTheStepAndTheGuestIdentifier(t *testing.T) {
	r := provisionRequest(Spec{StartAfterCreate: true})
	if err := r.Advance(time.Now()); err != nil { // → cloning
		t.Fatal(err)
	}
	r.VMID = "135"

	r.Fail(time.Now(), errors.New("storage pool is full"))

	if r.State != StateFailed {
		t.Errorf("state = %s, want failed", r.State)
	}
	if r.Step != string(StateCloning) {
		t.Errorf("step = %q, want the step that failed", r.Step)
	}
	if r.VMID != "135" {
		t.Error("the identifier of the partially created guest was discarded")
	}
	if r.Error == "" {
		t.Error("the cause was not recorded")
	}
}

func TestFinishedRequestsDoNotMove(t *testing.T) {
	r := provisionRequest(Spec{})
	r.State = StateReady

	if err := r.Advance(time.Now()); !errors.Is(err, ErrTerminal) {
		t.Errorf("advance on a finished request = %v, want ErrTerminal", err)
	}

	// A late failure report must not reopen a request that succeeded.
	r.Fail(time.Now(), errors.New("a straggling poll"))
	if r.State != StateReady {
		t.Errorf("state = %s, want the terminal state to hold", r.State)
	}
}

func TestValidateRejectsIncoherentRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *Request
	}{
		{"no platform", &Request{Kind: KindProvision, TemplateExternalID: "9000", GuestName: "x"}},
		{"no template", &Request{PlatformID: uuid.New(), Kind: KindProvision, GuestName: "x"}},
		{"no name", &Request{PlatformID: uuid.New(), Kind: KindProvision, TemplateExternalID: "9000"}},
		{"growth without a disk", &Request{
			PlatformID: uuid.New(), Kind: KindProvision, TemplateExternalID: "9000", GuestName: "x",
			Spec: Spec{DiskGrowBytes: 1 << 30},
		}},
		{"shrink", &Request{
			PlatformID: uuid.New(), Kind: KindProvision, TemplateExternalID: "9000", GuestName: "x",
			Spec: Spec{DiskGrowBytes: -1, DiskName: "scsi0"},
		}},
		{"destroy without an id", &Request{PlatformID: uuid.New(), Kind: KindDestroy}},
		{"unknown kind", &Request{PlatformID: uuid.New(), Kind: "migrate"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.req.Validate(); err == nil {
				t.Error("accepted a request that cannot be carried out")
			}
		})
	}

	ok := provisionRequest(Spec{DiskGrowBytes: 1 << 30, DiskName: "scsi0"})
	if err := ok.Validate(); err != nil {
		t.Errorf("rejected a valid request: %v", err)
	}
}

func templateRequest(spec Spec) *Request {
	spec.ImageURL = "https://cloud.example/debian-13-generic-amd64.qcow2"
	spec.ImageFile = "debian-13-generic-amd64.qcow2"
	spec.ImageStorage = "local"
	spec.Storage = "local-lvm"
	return &Request{
		ID: uuid.New(), PlatformID: uuid.New(), Kind: KindTemplate,
		State: StatePending, GuestName: "debian-13-cloud", TargetNode: "pve",
		Spec: spec,
	}
}

func TestBuildingATemplateWalksEveryStepInOrder(t *testing.T) {
	got := walk(t, templateRequest(Spec{}))

	// Preparing sits between importing and converting because a template
	// cannot be modified once it is one: the guest agent has to go in while
	// the disk still belongs to an ordinary guest.
	want := []State{
		StatePending, StateDownloading, StateCreating,
		StateImporting, StatePreparing, StateConverting, StateReady,
	}
	if len(got) != len(want) {
		t.Fatalf("path = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("path = %v, want %v", got, want)
		}
	}
}

// The image is hundreds of megabytes. Fetching it again because a second
// template is being built from it would cost minutes for nothing.
func TestAnImageAlreadyOnTheStorageIsNotDownloadedAgain(t *testing.T) {
	got := walk(t, templateRequest(Spec{ImagePresent: true}))

	for _, s := range got {
		if s == StateDownloading {
			t.Fatalf("an image already present was downloaded again: %v", got)
		}
	}
	if got[1] != StateCreating {
		t.Errorf("path = %v, want the download skipped straight to creating", got)
	}
	if got[len(got)-1] != StateReady {
		t.Errorf("finished at %s, want ready", got[len(got)-1])
	}
}

func TestTemplateValidationRejectsIncoherentBuilds(t *testing.T) {
	base := func() *Request { return templateRequest(Spec{}) }

	for _, tc := range []struct {
		name   string
		break_ func(*Request)
	}{
		{"no name", func(r *Request) { r.GuestName = "" }},
		{"no image", func(r *Request) { r.Spec.ImageURL = "" }},
		{"no node", func(r *Request) { r.TargetNode = "" }},
		{"no disk storage", func(r *Request) { r.Spec.Storage = "" }},
		{"no image storage", func(r *Request) { r.Spec.ImageStorage = "" }},
		// A digest with nothing to check it with, or an algorithm with nothing
		// to check — neither is a decision to skip verification, which is made
		// by leaving both empty.
		{"digest without algorithm", func(r *Request) { r.Spec.Checksum = "abc123" }},
		{"algorithm without digest", func(r *Request) { r.Spec.ChecksumAlgo = "sha512" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.break_(r)
			if err := r.Validate(); err == nil {
				t.Error("accepted a build that cannot be carried out")
			}
		})
	}

	verified := base()
	verified.Spec.Checksum = "abc123"
	verified.Spec.ChecksumAlgo = "sha512"
	if err := verified.Validate(); err != nil {
		t.Errorf("rejected a verified build: %v", err)
	}
	// Both empty is the deliberate skip, and it is legal here — it is refused
	// or audited a layer up, not made impossible.
	if err := base().Validate(); err != nil {
		t.Errorf("rejected an unverified build: %v", err)
	}
}
