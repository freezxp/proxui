package proxmox

// The provisioning calls are worth testing at the wire, because every bug this
// code can have is a bug about what exactly was sent: a resize read as absolute
// instead of relative, an SSH key list truncated at its first newline, a
// destroy without the purge flag that leaves backup jobs pointing at nothing.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/freezxp/proxui/internal/connector"
)

// capture records what the platform was asked to do.
type capture struct {
	method string
	path   string
	query  url.Values
	form   url.Values
}

// recordingServer answers every request with body and remembers the last one.
func recordingServer(t *testing.T, got *capture, body string) *Connector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		*got = capture{method: r.Method, path: r.URL.Path, query: r.URL.Query(), form: form}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	cfg := connector.Config{Endpoint: srv.URL}
	c, err := newClient(cfg, testCreds(), connector.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return &Connector{client: c, cfg: cfg}
}

func TestListTemplatesKeepsOnlyTemplatesAndFindsCloudInit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/cluster/resources"):
			_, _ = w.Write([]byte(`{"data":[
				{"type":"qemu","vmid":100,"name":"web-01","node":"pve","template":0,"maxdisk":10737418240},
				{"type":"qemu","vmid":9000,"name":"debian-13-cloud","node":"pve","template":1,"maxdisk":10737418240},
				{"type":"qemu","vmid":9001,"name":"bare-template","node":"pve2","template":1,"maxdisk":5368709120},
				{"type":"storage","storage":"local","node":"pve"}
			]}`))
		case strings.Contains(r.URL.Path, "/qemu/9000/config"):
			// The generated drive can be on any bus, so it is the value that
			// identifies it, not the key.
			_, _ = w.Write([]byte(`{"data":{"ide2":"local-lvm:vm-9000-cloudinit,media=cdrom","scsi0":"local-lvm:vm-9000-disk-0"}}`))
		case strings.Contains(r.URL.Path, "/qemu/9001/config"):
			_, _ = w.Write([]byte(`{"data":{"scsi0":"local-lvm:vm-9001-disk-0"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := connector.Config{Endpoint: srv.URL}
	c, err := newClient(cfg, testCreds(), connector.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	got, err := (&Connector{client: c, cfg: cfg}).ListTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("templates = %v, want the two with template=1", got)
	}
	byID := map[string]connector.TemplateRecord{}
	for _, tpl := range got {
		byID[tpl.ExternalID] = tpl
	}
	if !byID["9000"].HasCloudInit {
		t.Error("9000 has a cloud-init drive but was not flagged")
	}
	// Provisioning from a template with no cloud-init drive makes a machine
	// nobody can log into, so the flag has to be right in this direction too.
	if byID["9001"].HasCloudInit {
		t.Error("9001 has no cloud-init drive but was flagged as having one")
	}
	if byID["9001"].HostID != "pve2" {
		t.Errorf("node = %q, want pve2", byID["9001"].HostID)
	}
}

// Templates stay out of the inventory unless the operator asked otherwise —
// the option has been in the config schema since the connector was written.
func TestIncludeTemplatesControlsTheInventory(t *testing.T) {
	body := `{"data":[
		{"type":"qemu","vmid":100,"name":"web-01","node":"pve","status":"running","template":0},
		{"type":"qemu","vmid":9000,"name":"debian-13-cloud","node":"pve","status":"stopped","template":1}
	]}`

	for _, tc := range []struct {
		name  string
		extra map[string]any
		want  int
	}{
		{"default excludes templates", nil, 1},
		{"bool from the form", map[string]any{"include_templates": true}, 2},
		{"string from hand-written config", map[string]any{"include_templates": "true"}, 2},
		{"explicitly off", map[string]any{"include_templates": false}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)

			cfg := connector.Config{Endpoint: srv.URL, Extra: tc.extra}
			c, err := newClient(cfg, testCreds(), connector.Options{Timeout: 2 * time.Second})
			if err != nil {
				t.Fatalf("newClient: %v", err)
			}
			vms, err := (&Connector{client: c, cfg: cfg}).ListVMs(context.Background())
			if err != nil {
				t.Fatalf("ListVMs: %v", err)
			}
			if len(vms) != tc.want {
				t.Errorf("vms = %d, want %d", len(vms), tc.want)
			}
		})
	}
}

func TestCloneSendsTheTemplateAndTheNewIdentity(t *testing.T) {
	var got capture
	c := recordingServer(t, &got, `{"data":"UPID:pve:0000:clone::root@pam:"}`)

	task, err := c.Clone(context.Background(), connector.CloneSpec{
		Template:   connector.VMRef{ExternalID: "9000", HostID: "pve", Type: "qemu"},
		NewID:      "135",
		Name:       "web-02",
		FullClone:  true,
		Storage:    "local-lvm",
		TargetNode: "pve2",
	})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if task.ID == "" || task.Node != "pve" {
		t.Errorf("task = %+v, want a UPID on the template's node", task)
	}
	if !strings.HasSuffix(got.path, "/nodes/pve/qemu/9000/clone") {
		t.Errorf("path = %q", got.path)
	}
	for k, want := range map[string]string{
		"newid": "135", "name": "web-02", "full": "1",
		"storage": "local-lvm", "target": "pve2",
	} {
		if got.form.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.form.Get(k), want)
		}
	}
}

// Proxmox rejects a cross-node clone unless the template is on shared storage,
// so asking for the node it is already on must not send the parameter at all.
func TestCloneOmitsTargetWhenItIsTheTemplatesOwnNode(t *testing.T) {
	var got capture
	c := recordingServer(t, &got, `{"data":"UPID:pve:0000:clone::root@pam:"}`)

	if _, err := c.Clone(context.Background(), connector.CloneSpec{
		Template:   connector.VMRef{ExternalID: "9000", HostID: "pve", Type: "qemu"},
		NewID:      "135",
		TargetNode: "pve",
	}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, present := got.form["target"]; present {
		t.Errorf("target was sent for the template's own node: %q", got.form.Get("target"))
	}
}

func TestConfigureSendsCloudInitSettings(t *testing.T) {
	var got capture
	c := recordingServer(t, &got, `{"data":null}`)

	upgrade := false
	err := c.Configure(context.Background(),
		connector.VMRef{ExternalID: "135", HostID: "pve", Type: "qemu"},
		connector.CloudInitSpec{
			User:            "ubuntu",
			SSHKeys:         []string{"ssh-ed25519 AAAAC3Nza portal@proxui", "ssh-ed25519 AAAAC3Nzb someone@laptop"},
			IPConfig:        "ip=10.0.30.50/24,gw=10.0.30.1",
			Nameserver:      "10.0.30.1",
			Cores:           4,
			MemoryMB:        4096,
			Bridge:          "vmbr0",
			VLAN:            30,
			UpgradePackages: &upgrade,
			StartOnBoot:     true,
		})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !strings.HasSuffix(got.path, "/nodes/pve/qemu/135/config") {
		t.Errorf("path = %q", got.path)
	}
	for k, want := range map[string]string{
		"ciuser": "ubuntu", "ipconfig0": "ip=10.0.30.50/24,gw=10.0.30.1",
		"nameserver": "10.0.30.1", "cores": "4", "memory": "4096",
		"net0": "virtio,bridge=vmbr0,tag=30", "ciupgrade": "0", "onboot": "1",
	} {
		if got.form.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.form.Get(k), want)
		}
	}

	// The value Proxmox stores is URL-encoded, so both keys must survive the
	// newline between them. Sending them raw loses everything after the first.
	keys := got.form.Get("sshkeys")
	if strings.Contains(keys, "\n") {
		t.Errorf("sshkeys was sent raw: %q", keys)
	}
	// Proxmox's urlencoded validator refuses "+" as a space and says only
	// "invalid urlencoded string", which is a long way from the cause.
	if strings.Contains(keys, "+") {
		t.Errorf("sshkeys used form-style escaping: %q", keys)
	}
	if !strings.Contains(keys, "%20") {
		t.Errorf("sshkeys = %q, want spaces percent-encoded", keys)
	}
	decoded, err := url.QueryUnescape(keys)
	if err != nil {
		t.Fatalf("sshkeys is not URL-encoded: %v", err)
	}
	if lines := strings.Split(decoded, "\n"); len(lines) != 2 {
		t.Errorf("sshkeys decoded to %d keys, want 2: %q", len(lines), decoded)
	}
}

// A spec that asks for nothing must not write an empty config, which Proxmox
// answers with an error about a missing parameter.
func TestConfigureWithNothingToSetDoesNotCall(t *testing.T) {
	var got capture
	c := recordingServer(t, &got, `{"data":null}`)

	if err := c.Configure(context.Background(),
		connector.VMRef{ExternalID: "135", HostID: "pve"}, connector.CloudInitSpec{}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if got.method != "" {
		t.Errorf("an empty spec still called %s %s", got.method, got.path)
	}
}

func TestResizeGrowsRelativelyAndRefusesToShrink(t *testing.T) {
	var got capture
	c := recordingServer(t, &got, `{"data":null}`)

	vm := connector.VMRef{ExternalID: "135", HostID: "pve", Type: "qemu"}
	if err := c.ResizeDisk(context.Background(), vm, "scsi0", 20*1024*1024*1024); err != nil {
		t.Fatalf("ResizeDisk: %v", err)
	}
	if got.form.Get("disk") != "scsi0" {
		t.Errorf("disk = %q", got.form.Get("disk"))
	}
	// Without the leading "+" the number is a target size, and a value below
	// the current disk is refused rather than applied — a silently unresized
	// guest is worse than a loud failure.
	if size := got.form.Get("size"); !strings.HasPrefix(size, "+") {
		t.Errorf("size = %q, want a relative growth", size)
	}
	if got.method != http.MethodPut {
		t.Errorf("method = %s, want PUT", got.method)
	}

	for _, bad := range []int64{0, -1} {
		if err := c.ResizeDisk(context.Background(), vm, "scsi0", bad); err == nil {
			t.Errorf("growing by %d was accepted; disks cannot shrink", bad)
		}
	}
}

func TestDestroyPurgesReferencesAndUnreferencedDisks(t *testing.T) {
	var got capture
	c := recordingServer(t, &got, `{"data":"UPID:pve:0000:qmdestroy::root@pam:"}`)

	task, err := c.Destroy(context.Background(),
		connector.VMRef{ExternalID: "135", HostID: "pve", Type: "qemu"},
		connector.DestroyOptions{PurgeReferences: true, DestroyUnreferencedDisks: true})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if task.ID == "" || task.Node != "pve" {
		t.Errorf("task = %+v", task)
	}
	if got.method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", got.method)
	}
	// Without purge the guest goes and the backup jobs naming it stay, failing
	// nightly against a VMID that no longer exists.
	if got.query.Get("purge") != "1" {
		t.Error("purge was not requested")
	}
	if got.query.Get("destroy-unreferenced-disks") != "1" {
		t.Error("unreferenced disks would have been left behind")
	}
}

func TestNextIDAcceptsBothShapesProxmoxReturns(t *testing.T) {
	for _, body := range []string{`{"data":"135"}`, `{"data":135}`} {
		var got capture
		c := recordingServer(t, &got, body)
		id, err := c.NextID(context.Background())
		if err != nil {
			t.Fatalf("NextID(%s): %v", body, err)
		}
		if id != "135" {
			t.Errorf("NextID(%s) = %q, want 135", body, id)
		}
	}
}

// Provisioning privileges are a capability, not a requirement: a token without
// them describes a working read-and-console platform.
func TestProvisioningPrivilegesAreReportedSeparately(t *testing.T) {
	readOnly := permissionMap{"/": {"VM.Audit": 1, "Sys.Audit": 1, "Datastore.Audit": 1}}
	if missing := missingProvisioningPrivileges(readOnly); len(missing) == 0 {
		t.Error("a read-only token was reported as able to provision")
	}
	if missing := missingPrivileges(readOnly); len(missing) == 0 {
		t.Error("expected the console and power privileges to be reported missing too")
	}

	widened := permissionMap{"/": {
		"VM.Audit": 1, "Sys.Audit": 1, "Datastore.Audit": 1, "VM.Console": 1,
		"VM.PowerMgmt": 1, "VM.GuestAgent.Audit": 1,
		"VM.Allocate": 1, "VM.Clone": 1, "VM.Config.Disk": 1, "VM.Config.CPU": 1,
		"VM.Config.Memory": 1, "VM.Config.Network": 1, "VM.Config.Options": 1,
		"VM.Config.Cloudinit": 1, "Datastore.AllocateSpace": 1, "SDN.Use": 1,
	}}
	if missing := missingProvisioningPrivileges(widened); len(missing) != 0 {
		t.Errorf("a widened token still reported missing: %v", missing)
	}
}

func TestImageExistsMatchesTheStoredVolume(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Only the import content is asked for; anything else would be the
		// wrong question and would match ISOs by accident.
		if r.URL.Query().Get("content") != "import" {
			t.Errorf("content filter = %q, want import", r.URL.Query().Get("content"))
		}
		_, _ = w.Write([]byte(`{"data":[
			{"volid":"local:import/debian-13-generic-amd64.qcow2","size":351272960},
			{"volid":"local:import/other.qcow2","size":100}
		]}`))
	}))
	t.Cleanup(srv.Close)

	cfg := connector.Config{Endpoint: srv.URL}
	cl, err := newClient(cfg, testCreds(), connector.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	c := &Connector{client: cl, cfg: cfg}

	for name, want := range map[string]bool{
		"debian-13-generic-amd64.qcow2": true,
		"other.qcow2":                   true,
		"ubuntu-24.04-server.img":       false,
		// A suffix that is not a whole filename must not match: "amd64.qcow2"
		// is a substring of the stored name but a different file.
		"amd64.qcow2": false,
	} {
		got, err := c.ImageExists(context.Background(), "pve", "local", name)
		if err != nil {
			t.Fatalf("ImageExists(%s): %v", name, err)
		}
		if got != want {
			t.Errorf("ImageExists(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestDownloadImageCarriesTheChecksum(t *testing.T) {
	var got capture
	c := recordingServer(t, &got, `{"data":"UPID:pve:0000:download::root@pam:"}`)

	task, err := c.DownloadImage(context.Background(), connector.ImageDownloadSpec{
		Node: "pve", Storage: "local",
		URL:               "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-generic-amd64.qcow2",
		Filename:          "debian-13-generic-amd64.qcow2",
		Checksum:          "abc123",
		ChecksumAlgorithm: "sha512",
	})
	if err != nil {
		t.Fatalf("DownloadImage: %v", err)
	}
	if task.ID == "" || task.Node != "pve" {
		t.Errorf("task = %+v", task)
	}
	if !strings.HasSuffix(got.path, "/nodes/pve/storage/local/download-url") {
		t.Errorf("path = %q", got.path)
	}
	for k, want := range map[string]string{
		"content": "import", "filename": "debian-13-generic-amd64.qcow2",
		"checksum": "abc123", "checksum-algorithm": "sha512",
	} {
		if got.form.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.form.Get(k), want)
		}
	}
	// Certificate verification is on at the platform by default; the portal
	// must not quietly turn it off.
	if _, present := got.form["verify-certificates"]; present {
		t.Errorf("verify-certificates was sent unasked: %q", got.form.Get("verify-certificates"))
	}
}

// An unverified download is legal — it is refused or audited a layer up — but a
// half-specified one cannot be checked and is a mistake worth catching here.
func TestDownloadImageRejectsAHalfSpecifiedChecksum(t *testing.T) {
	var got capture
	c := recordingServer(t, &got, `{"data":"UPID:..."}`)

	base := connector.ImageDownloadSpec{
		Node: "pve", Storage: "local", URL: "https://x/y.qcow2", Filename: "y.qcow2",
	}
	for _, bad := range []connector.ImageDownloadSpec{
		func() connector.ImageDownloadSpec { s := base; s.Checksum = "abc"; return s }(),
		func() connector.ImageDownloadSpec { s := base; s.ChecksumAlgorithm = "sha512"; return s }(),
	} {
		if _, err := c.DownloadImage(context.Background(), bad); err == nil {
			t.Error("accepted a checksum that could never be verified")
		}
	}

	// Neither set is the deliberate skip, and it goes through.
	if _, err := c.DownloadImage(context.Background(), base); err != nil {
		t.Errorf("an unverified download was refused: %v", err)
	}
}

// Cloud images log to serial and many ship without a framebuffer, so a template
// built without a serial console produces guests whose console is a blank
// screen — the single most common way this goes wrong by hand.
func TestCreateGuestGivesACloudImageWhatItNeeds(t *testing.T) {
	var got capture
	c := recordingServer(t, &got, `{"data":"UPID:pve:0000:create::root@pam:"}`)

	if _, err := c.CreateGuest(context.Background(), connector.GuestCreateSpec{
		Node: "pve", VMID: "9000", Name: "debian-13-cloud", Bridge: "vmbr0",
	}); err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	if !strings.HasSuffix(got.path, "/nodes/pve/qemu") {
		t.Errorf("path = %q", got.path)
	}
	for k, want := range map[string]string{
		"vmid": "9000", "name": "debian-13-cloud", "ostype": "l26",
		"scsihw": "virtio-scsi-single", "agent": "1",
		"serial0": "socket", "vga": "serial0", "net0": "virtio,bridge=vmbr0",
	} {
		if got.form.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.form.Get(k), want)
		}
	}
	// Sizing an unset field to zero would be rejected by the platform.
	if got.form.Get("cores") == "0" || got.form.Get("memory") == "0" {
		t.Error("a guest was created with zero cores or memory")
	}
}

// The disk and the cloud-init drive go in one request. Two would leave a window
// where the template has no way to take a user or a key, and nothing would ever
// come back to fix it.
func TestImportDiskAttachesTheCloudInitDriveInTheSameRequest(t *testing.T) {
	var got capture
	c := recordingServer(t, &got, `{"data":"UPID:pve:0000:import::root@pam:"}`)

	if _, err := c.ImportDisk(context.Background(),
		connector.VMRef{ExternalID: "9000", HostID: "pve", Type: "qemu"},
		connector.DiskImportSpec{
			Disk: "scsi0", Storage: "local-lvm",
			SourceVolume:   "local:import/debian-13-generic-amd64.qcow2",
			CloudInitDrive: "ide2",
		}); err != nil {
		t.Fatalf("ImportDisk: %v", err)
	}
	if want := "local-lvm:0,import-from=local:import/debian-13-generic-amd64.qcow2"; got.form.Get("scsi0") != want {
		t.Errorf("scsi0 = %q, want %q", got.form.Get("scsi0"), want)
	}
	if got.form.Get("ide2") != "local-lvm:cloudinit" {
		t.Errorf("ide2 = %q, want the generated cloud-init drive", got.form.Get("ide2"))
	}
	// Without a boot order the guest may try to boot the cloud-init drive.
	if got.form.Get("boot") != "order=scsi0" {
		t.Errorf("boot = %q, want order=scsi0", got.form.Get("boot"))
	}
}

func TestConvertToTemplateHitsTheTemplateEndpoint(t *testing.T) {
	var got capture
	c := recordingServer(t, &got, `{"data":"UPID:pve:0000:template::root@pam:"}`)

	if _, err := c.ConvertToTemplate(context.Background(),
		connector.VMRef{ExternalID: "9000", HostID: "pve", Type: "qemu"}); err != nil {
		t.Fatalf("ConvertToTemplate: %v", err)
	}
	if !strings.HasSuffix(got.path, "/nodes/pve/qemu/9000/template") {
		t.Errorf("path = %q", got.path)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
}

// Building a template needs strictly more than cloning from one, so a token
// that can provision is not necessarily one that can build.
func TestTemplatePrivilegesAreReportedApartFromProvisioning(t *testing.T) {
	canProvision := permissionMap{"/": {
		"VM.Allocate": 1, "VM.Clone": 1, "VM.Config.Disk": 1, "VM.Config.CPU": 1,
		"VM.Config.Memory": 1, "VM.Config.Network": 1, "VM.Config.Options": 1,
		"VM.Config.Cloudinit": 1, "Datastore.AllocateSpace": 1, "SDN.Use": 1,
	}}
	if missing := missingProvisioningPrivileges(canProvision); len(missing) != 0 {
		t.Fatalf("provisioning reported missing: %v", missing)
	}
	if missing := missingTemplatePrivileges(canProvision); len(missing) != 4 {
		t.Errorf("template privileges missing = %v, want all four", missing)
	}

	// Building is a superset: everything provisioning needs, plus four.
	canBuild := permissionMap{"/": {
		"VM.Allocate": 1, "VM.Clone": 1, "VM.Config.Disk": 1, "VM.Config.CPU": 1,
		"VM.Config.Memory": 1, "VM.Config.Network": 1, "VM.Config.Options": 1,
		"VM.Config.Cloudinit": 1, "Datastore.AllocateSpace": 1, "SDN.Use": 1,
		"Datastore.AllocateTemplate": 1, "Sys.AccessNetwork": 1,
		"VM.Config.CDROM": 1, "VM.Config.HWType": 1,
	}}
	if missing := missingTemplatePrivileges(canBuild); len(missing) != 0 {
		t.Errorf("a token holding all three still reported missing: %v", missing)
	}
	// Sys.Modify is the broad alternative the API also accepts. The portal does
	// not ask for it, so holding it must not be mistaken for holding the narrow
	// privilege that is actually requested.
	if missing := missingTemplatePrivileges(permissionMap{"/": {"Sys.Modify": 1}}); len(missing) != 14 {
		t.Errorf("Sys.Modify was treated as a substitute: %v", missing)
	}
}
