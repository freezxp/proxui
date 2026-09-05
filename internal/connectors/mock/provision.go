package mock

import (
	"context"
	"fmt"
	"strconv"

	"github.com/freezxp/proxui/internal/connector"
)

// The fake platform can create and destroy guests (ADR 0010), so the whole
// provisioning path — the request state machine, the driver's step ordering,
// the job that reschedules it, the group filing at the end — runs in CI with no
// Proxmox anywhere. That is the promise docs/09 §9.5 makes about the mock, and
// provisioning is exactly the kind of feature it would be embarrassing to have
// only ever exercised by hand against a real cluster.
//
// What it deliberately does not simulate is the platform's own refusals: a full
// storage pool, a VMID collision, a lock held by another task. Those belong to
// the connector that talks to a real platform, and are tested there against a
// real server.

// mockTaskPrefix marks the task handles this connector hands out.
const mockTaskPrefix = "mock-task:"

// ListTemplates implements connector.Provisioner.
func (c *Connector) ListTemplates(ctx context.Context) ([]connector.TemplateRecord, error) {
	if err := c.gate(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	node := "node01"
	if len(c.hosts) > 0 {
		node = c.hosts[0].ExternalID
	}
	built := append([]connector.TemplateRecord(nil), c.templates...)
	return append(built, []connector.TemplateRecord{
		{
			ExternalID: "9000", Name: "mock-cloudinit-template", Type: "qemu",
			HostID: node, DiskBytes: 10 << 30, HasCloudInit: true,
		},
		{
			// One without a cloud-init drive, so the case an operator must be
			// warned about is representable rather than hypothetical.
			ExternalID: "9001", Name: "mock-bare-template", Type: "qemu",
			HostID: node, DiskBytes: 8 << 30, HasCloudInit: false,
		},
	}...), nil
}

// NextID implements connector.Provisioner.
func (c *Connector) NextID(ctx context.Context) (string, error) {
	if err := c.gate(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	highest := 100
	for _, vm := range c.vms {
		if n, err := strconv.Atoi(vm.ExternalID); err == nil && n > highest {
			highest = n
		}
	}
	return strconv.Itoa(highest + 1), nil
}

// Clone implements connector.Provisioner, adding a guest to the fake fleet.
//
// It appears stopped and unconfigured, which is what a real clone produces:
// the settings arrive in the Configure step and the machine boots after that.
func (c *Connector) Clone(ctx context.Context, spec connector.CloneSpec) (connector.TaskRef, error) {
	if err := c.gate(ctx); err != nil {
		return connector.TaskRef{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, vm := range c.vms {
		if vm.ExternalID == spec.NewID {
			// The same collision a real platform reports when two requests are
			// handed the same free identifier.
			return connector.TaskRef{}, connector.Errorf(connector.ErrRefused, "clone",
				"identifier %s is already in use", spec.NewID)
		}
	}

	node := spec.TargetNode
	if node == "" {
		node = spec.Template.HostID
	}
	c.vms = append(c.vms, connector.VMRecord{
		ExternalID:  spec.NewID,
		Name:        spec.Name,
		Type:        "qemu",
		State:       "stopped",
		HostID:      node,
		CPUCores:    1,
		MemoryBytes: 1 << 30,
		DiskBytes:   10 << 30,
	})
	return connector.TaskRef{ID: mockTaskPrefix + "clone:" + spec.NewID, Node: node}, nil
}

// Configure implements connector.Provisioner.
func (c *Connector) Configure(ctx context.Context, vm connector.VMRef, spec connector.CloudInitSpec) error {
	if err := c.gate(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.vms {
		if c.vms[i].ExternalID != vm.ExternalID {
			continue
		}
		if spec.Cores > 0 {
			c.vms[i].CPUCores = spec.Cores
		}
		if spec.MemoryMB > 0 {
			c.vms[i].MemoryBytes = int64(spec.MemoryMB) << 20
		}
		return nil
	}
	return connector.Errorf(connector.ErrNotSupported, "configure", "unknown guest %q", vm.ExternalID)
}

// ResizeDisk implements connector.Provisioner. Growth only, like the real one.
func (c *Connector) ResizeDisk(ctx context.Context, vm connector.VMRef, disk string, growBytes int64) error {
	if err := c.gate(ctx); err != nil {
		return err
	}
	if growBytes <= 0 {
		return connector.Errorf(connector.ErrNotSupported, "resize",
			"disks can only be grown; asked to change %s by %d bytes", disk, growBytes)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.vms {
		if c.vms[i].ExternalID == vm.ExternalID {
			c.vms[i].DiskBytes += growBytes
			return nil
		}
	}
	return connector.Errorf(connector.ErrNotSupported, "resize", "unknown guest %q", vm.ExternalID)
}

// Destroy implements connector.Destroyer.
func (c *Connector) Destroy(ctx context.Context, vm connector.VMRef, _ connector.DestroyOptions) (connector.TaskRef, error) {
	if err := c.gate(ctx); err != nil {
		return connector.TaskRef{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, existing := range c.vms {
		if existing.ExternalID != vm.ExternalID {
			continue
		}
		// A real platform refuses to remove a running guest, and the portal
		// leaves that judgement to it — so the fake has to make it too, or the
		// path that reports the refusal is never exercised.
		if existing.State == "running" {
			return connector.TaskRef{}, connector.Errorf(connector.ErrRefused, "destroy",
				"guest %s is running", vm.ExternalID)
		}
		c.vms = append(c.vms[:i], c.vms[i+1:]...)
		return connector.TaskRef{ID: mockTaskPrefix + "destroy:" + vm.ExternalID, Node: vm.HostID}, nil
	}
	return connector.TaskRef{}, connector.Errorf(connector.ErrNotSupported, "destroy",
		"unknown guest %q", vm.ExternalID)
}

// TaskState implements connector.TaskWatcher.
//
// Every task is already finished: the fake does its work synchronously, and
// pretending otherwise would only add a wait to every test without exercising
// anything the driver does differently.
func (c *Connector) TaskState(ctx context.Context, task connector.TaskRef) (bool, bool, string, error) {
	if err := c.gate(ctx); err != nil {
		return false, false, "", err
	}
	if task.ID == "" {
		return false, false, "", connector.Errorf(connector.ErrInvalidConfig, "task",
			"task reference is incomplete")
	}
	return true, true, fmt.Sprintf("OK (%s)", task.ID), nil
}

// Template building on the fake platform, so the whole path — download skip,
// step ordering, the request surviving a restart — runs in CI with no Proxmox.
//
// The fleet remembers which images have been "downloaded" so the skip is
// exercised for real rather than asserted about a constant.

// ImageExists implements connector.TemplateBuilder.
func (c *Connector) ImageExists(ctx context.Context, _, _, filename string) (bool, error) {
	if err := c.gate(ctx); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.images[filename], nil
}

// DownloadImage implements connector.TemplateBuilder.
func (c *Connector) DownloadImage(ctx context.Context, spec connector.ImageDownloadSpec) (connector.TaskRef, error) {
	if err := c.gate(ctx); err != nil {
		return connector.TaskRef{}, err
	}
	if (spec.Checksum == "") != (spec.ChecksumAlgorithm == "") {
		return connector.TaskRef{}, connector.Errorf(connector.ErrInvalidConfig, "download_image",
			"a checksum needs both a digest and an algorithm")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.images == nil {
		c.images = map[string]bool{}
	}
	c.images[spec.Filename] = true
	return connector.TaskRef{ID: mockTaskPrefix + "download:" + spec.Filename, Node: spec.Node}, nil
}

// CreateGuest implements connector.TemplateBuilder.
func (c *Connector) CreateGuest(ctx context.Context, spec connector.GuestCreateSpec) (connector.TaskRef, error) {
	if err := c.gate(ctx); err != nil {
		return connector.TaskRef{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, vm := range c.vms {
		if vm.ExternalID == spec.VMID {
			return connector.TaskRef{}, connector.Errorf(connector.ErrRefused, "create_guest",
				"identifier %s is already in use", spec.VMID)
		}
	}
	c.vms = append(c.vms, connector.VMRecord{
		ExternalID: spec.VMID, Name: spec.Name, Type: "qemu", State: "stopped",
		HostID: spec.Node, CPUCores: spec.Cores, MemoryBytes: int64(spec.MemoryMB) << 20,
	})
	return connector.TaskRef{ID: mockTaskPrefix + "create:" + spec.VMID, Node: spec.Node}, nil
}

// ImportDisk implements connector.TemplateBuilder.
func (c *Connector) ImportDisk(ctx context.Context, vm connector.VMRef, spec connector.DiskImportSpec) (connector.TaskRef, error) {
	if err := c.gate(ctx); err != nil {
		return connector.TaskRef{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.vms {
		if c.vms[i].ExternalID == vm.ExternalID {
			c.vms[i].DiskBytes = 10 << 30
			return connector.TaskRef{ID: mockTaskPrefix + "import:" + vm.ExternalID, Node: vm.HostID}, nil
		}
	}
	return connector.TaskRef{}, connector.Errorf(connector.ErrNotSupported, "import_disk",
		"unknown guest %q", vm.ExternalID)
}

// ConvertToTemplate implements connector.TemplateBuilder.
//
// The converted guest leaves ListVMs and appears in ListTemplates, which is the
// behaviour the portal depends on: a template counted as a stopped VM would
// mislead anyone reading a fleet total.
func (c *Connector) ConvertToTemplate(ctx context.Context, vm connector.VMRef) (connector.TaskRef, error) {
	if err := c.gate(ctx); err != nil {
		return connector.TaskRef{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, existing := range c.vms {
		if existing.ExternalID != vm.ExternalID {
			continue
		}
		c.templates = append(c.templates, connector.TemplateRecord{
			ExternalID: existing.ExternalID, Name: existing.Name, Type: "qemu",
			HostID: existing.HostID, DiskBytes: existing.DiskBytes, HasCloudInit: true,
		})
		c.vms = append(c.vms[:i], c.vms[i+1:]...)
		return connector.TaskRef{ID: mockTaskPrefix + "template:" + vm.ExternalID, Node: vm.HostID}, nil
	}
	return connector.TaskRef{}, connector.Errorf(connector.ErrNotSupported, "convert_template",
		"unknown guest %q", vm.ExternalID)
}
