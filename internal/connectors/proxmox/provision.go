package proxmox

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/freezxp/proxui/internal/connector"
)

// Provisioning against Proxmox (ADR 0010).
//
// The four steps are four calls with different shapes, and the difference is
// why the connector contract keeps them apart rather than offering one
// "create a VM": clone answers with a UPID naming a task that may run for
// minutes, while config and resize answer when they are done. The engine that
// drives them needs to know which is which.

// cloudInitDrivePattern is how Proxmox names the drive it generates. It appears
// as a value like "local-lvm:vm-9000-cloudinit,media=cdrom" against whichever
// bus the template used, so the key is not predictable but the value is.
const cloudInitDriveMarker = "cloudinit"

// ListTemplates implements connector.Provisioner.
//
// Templates are absent from ListVMs on purpose — counting them as stopped
// machines misleads anyone reading a fleet total — so this is the only place
// the core learns they exist.
func (c *Connector) ListTemplates(ctx context.Context) ([]connector.TemplateRecord, error) {
	resources, err := c.resources(ctx)
	if err != nil {
		return nil, err
	}

	var out []connector.TemplateRecord
	for _, r := range resources {
		if r.Template != 1 || (r.Type != "qemu" && r.Type != "lxc") {
			continue
		}
		rec := connector.TemplateRecord{
			ExternalID: fmt.Sprintf("%d", r.VMID),
			Name:       firstNonEmpty(r.Name, fmt.Sprintf("template-%d", r.VMID)),
			Type:       r.Type,
			HostID:     r.Node,
			DiskBytes:  r.MaxDisk,
		}
		// Whether the template can take a user and an SSH key at all is the
		// one thing an operator cannot see from its name, and provisioning
		// from a template without a cloud-init drive produces a machine nobody
		// can log into. A config we cannot read leaves the flag false, which
		// is the cautious direction.
		if hasCI, err := c.hasCloudInitDrive(ctx, r.Node, r.Type, rec.ExternalID); err == nil {
			rec.HasCloudInit = hasCI
		}
		out = append(out, rec)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// hasCloudInitDrive reports whether a guest carries the generated cloud-init
// drive. The drive can be on any bus, so the values are searched rather than a
// particular key being read.
func (c *Connector) hasCloudInitDrive(ctx context.Context, node, guestType, vmid string) (bool, error) {
	if guestType == "" {
		guestType = "qemu"
	}
	var cfg map[string]any
	path := fmt.Sprintf("/nodes/%s/%s/%s/config", node, guestType, vmid)
	if err := c.client.get(ctx, path, &cfg); err != nil {
		return false, err
	}
	for _, v := range cfg {
		if s, ok := v.(string); ok && strings.Contains(s, cloudInitDriveMarker) {
			return true, nil
		}
	}
	return false, nil
}

// NextID implements connector.Provisioner.
//
// Proxmox reserves nothing: this reports an id that was free at the moment it
// was asked. Two requests in flight can be handed the same one, so Clone treats
// a rejection as ordinary and the caller asks again.
func (c *Connector) NextID(ctx context.Context) (string, error) {
	var id any
	if err := c.client.get(ctx, "/cluster/nextid", &id); err != nil {
		return "", err
	}
	switch v := id.(type) {
	case string:
		return v, nil
	case float64:
		return strconv.FormatInt(int64(v), 10), nil
	default:
		return "", connector.Errorf(connector.ErrUnreachable, "nextid",
			"platform returned an unusable identifier %v", id)
	}
}

// Clone implements connector.Provisioner. It returns as soon as the platform
// has accepted the task; copying disks can take minutes.
func (c *Connector) Clone(ctx context.Context, spec connector.CloneSpec) (connector.TaskRef, error) {
	if spec.Template.ExternalID == "" || spec.Template.HostID == "" {
		return connector.TaskRef{}, connector.Errorf(connector.ErrInvalidConfig, "clone",
			"cloning needs both the template's node and its id")
	}
	if spec.NewID == "" {
		return connector.TaskRef{}, connector.Errorf(connector.ErrInvalidConfig, "clone",
			"cloning needs an identifier for the new guest")
	}
	guestType := spec.Template.Type
	if guestType == "" {
		guestType = "qemu"
	}

	form := url.Values{}
	form.Set("newid", spec.NewID)
	if spec.Name != "" {
		form.Set("name", spec.Name)
	}
	if spec.FullClone {
		form.Set("full", "1")
	}
	if spec.Storage != "" {
		form.Set("storage", spec.Storage)
	}
	// Only legal when the template sits on shared storage; Proxmox refuses it
	// otherwise, and its refusal says so more precisely than a pre-check here
	// could.
	if spec.TargetNode != "" && spec.TargetNode != spec.Template.HostID {
		form.Set("target", spec.TargetNode)
	}
	if spec.Description != "" {
		form.Set("description", spec.Description)
	}

	path := fmt.Sprintf("/nodes/%s/%s/%s/clone", spec.Template.HostID, guestType, spec.Template.ExternalID)
	var upid string
	if err := c.client.post(ctx, path, form, &upid); err != nil {
		return connector.TaskRef{}, err
	}
	return connector.TaskRef{ID: upid, Node: spec.Template.HostID}, nil
}

// Configure implements connector.Provisioner, writing the cloud-init settings
// and the hardware sizing onto a guest that has just been cloned.
//
// Synchronous: the platform answers when the config file is written. The guest
// reads it on its next boot, which is why provisioning configures before it
// starts rather than after.
func (c *Connector) Configure(ctx context.Context, vm connector.VMRef, spec connector.CloudInitSpec) error {
	if vm.ExternalID == "" || vm.HostID == "" {
		return connector.Errorf(connector.ErrInvalidConfig, "configure",
			"configuring needs both the node and the guest id")
	}
	guestType := vm.Type
	if guestType == "" {
		guestType = "qemu"
	}

	form := url.Values{}
	if spec.User != "" {
		form.Set("ciuser", spec.User)
	}
	if len(spec.SSHKeys) > 0 {
		form.Set("sshkeys", encodeSSHKeys(spec.SSHKeys))
	}
	if spec.IPConfig != "" {
		form.Set("ipconfig0", spec.IPConfig)
	}
	if spec.Nameserver != "" {
		form.Set("nameserver", spec.Nameserver)
	}
	if spec.SearchDomain != "" {
		form.Set("searchdomain", spec.SearchDomain)
	}
	if spec.Cores > 0 {
		form.Set("cores", strconv.Itoa(spec.Cores))
	}
	if spec.MemoryMB > 0 {
		form.Set("memory", strconv.Itoa(spec.MemoryMB))
	}
	if spec.Bridge != "" {
		net := "virtio,bridge=" + spec.Bridge
		if spec.VLAN > 0 {
			net += ",tag=" + strconv.Itoa(spec.VLAN)
		}
		form.Set("net0", net)
	}
	if spec.UpgradePackages != nil {
		form.Set("ciupgrade", boolParam(*spec.UpgradePackages))
	}
	if spec.StartOnBoot {
		form.Set("onboot", "1")
	}
	if len(form) == 0 {
		return nil
	}

	path := fmt.Sprintf("/nodes/%s/%s/%s/config", vm.HostID, guestType, vm.ExternalID)
	return c.client.post(ctx, path, form, nil)
}

// encodeSSHKeys prepares the sshkeys parameter.
//
// The parameter is declared `format => 'urlencoded'` in PVE::QemuServer, so the
// key material is escaped before it goes into the form — where the form encoder
// escapes it again. The double encoding looks like a mistake and is the
// contract: without it the newline between two keys terminates the value and
// every key after the first is silently lost.
//
// It has to be percent-encoding throughout, though. url.QueryEscape writes a
// space as "+", which is correct for form bodies and rejected outright by
// Proxmox's validator — "invalid urlencoded string" on a key that is perfectly
// well formed. Found by provisioning a guest against a live cluster; the shape
// of the value is right either way, so nothing short of the platform's own
// parser would have said otherwise.
func encodeSSHKeys(keys []string) string {
	cleaned := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			cleaned = append(cleaned, k)
		}
	}
	return strings.ReplaceAll(url.QueryEscape(strings.Join(cleaned, "\n")), "+", "%20")
}

func boolParam(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ResizeDisk implements connector.Provisioner.
//
// Growth only. Proxmox will not shrink a disk, and a caller asking for one is
// making a mistake worth reporting rather than quietly rounding to zero — a
// silently ignored resize produces a guest with the template's disk and an
// operator who believes otherwise.
func (c *Connector) ResizeDisk(ctx context.Context, vm connector.VMRef, disk string, growBytes int64) error {
	if vm.ExternalID == "" || vm.HostID == "" {
		return connector.Errorf(connector.ErrInvalidConfig, "resize",
			"resizing needs both the node and the guest id")
	}
	if disk == "" {
		return connector.Errorf(connector.ErrInvalidConfig, "resize", "resizing needs a disk name, e.g. scsi0")
	}
	if growBytes <= 0 {
		return connector.Errorf(connector.ErrNotSupported, "resize",
			"disks can only be grown; asked to change %s by %d bytes", disk, growBytes)
	}

	guestType := vm.Type
	if guestType == "" {
		guestType = "qemu"
	}
	form := url.Values{}
	form.Set("disk", disk)
	// The leading "+" is what makes this relative. Without it the number is
	// read as a target size, and a value smaller than the current disk is
	// refused rather than applied.
	form.Set("size", fmt.Sprintf("+%dK", growBytes/1024))

	path := fmt.Sprintf("/nodes/%s/%s/%s/resize", vm.HostID, guestType, vm.ExternalID)
	return c.client.put(ctx, path, form, nil)
}

// Destroy implements connector.Destroyer.
//
// Asynchronous, like clone: removing disks takes as long as the storage takes.
// Proxmox refuses to destroy a running guest, and that refusal is left to it —
// it is the authority on whether the guest is running, and it says so more
// precisely than a pre-check against possibly stale inventory.
func (c *Connector) Destroy(ctx context.Context, vm connector.VMRef, opts connector.DestroyOptions) (connector.TaskRef, error) {
	if vm.ExternalID == "" || vm.HostID == "" {
		return connector.TaskRef{}, connector.Errorf(connector.ErrInvalidConfig, "destroy",
			"destroying needs both the node and the guest id")
	}
	guestType := vm.Type
	if guestType == "" {
		guestType = "qemu"
	}

	query := url.Values{}
	if opts.PurgeReferences {
		query.Set("purge", "1")
	}
	if opts.DestroyUnreferencedDisks {
		query.Set("destroy-unreferenced-disks", "1")
	}

	path := fmt.Sprintf("/nodes/%s/%s/%s", vm.HostID, guestType, vm.ExternalID)
	var upid string
	if err := c.client.del(ctx, path, query, &upid); err != nil {
		return connector.TaskRef{}, err
	}
	return connector.TaskRef{ID: upid, Node: vm.HostID}, nil
}
