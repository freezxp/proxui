package proxmox

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"context"

	"github.com/freezxp/proxui/internal/connector"
)

// Building a cloud-init template on Proxmox (ADR 0010).
//
// Four calls, all of them tasks the platform runs on its own time. The image is
// fetched by the node rather than by the portal: the node has the bandwidth and
// the storage, and download-url exists precisely so a client does not have to
// stream the file through itself.
//
// Endpoints and parameter names were read from the live cluster with
// `pvesh usage` and from the API source on the node.

// importContent is the storage content type a downloaded disk image is filed
// under. Proxmox distinguishes it from `iso`, and only `import` volumes can be
// used as the source of an import-from.
const importContent = "import"

// ImageExists implements connector.TemplateBuilder.
//
// The listing is per storage and per content type, so this asks exactly the
// question worth asking — "is this file already here" — rather than fetching
// hundreds of megabytes to find out.
func (c *Connector) ImageExists(ctx context.Context, node, storage, filename string) (bool, error) {
	if node == "" || storage == "" || filename == "" {
		return false, connector.Errorf(connector.ErrInvalidConfig, "image_exists",
			"checking for an image needs a node, a storage and a filename")
	}

	var content []struct {
		VolID string `json:"volid"`
	}
	path := fmt.Sprintf("/nodes/%s/storage/%s/content", node, storage)
	if err := c.client.getQuery(ctx, path, url.Values{"content": []string{importContent}}, &content); err != nil {
		return false, err
	}
	for _, v := range content {
		// A volid looks like "local:import/debian-13-generic-amd64.qcow2".
		if strings.HasSuffix(v.VolID, "/"+filename) {
			return true, nil
		}
	}
	return false, nil
}

// DownloadImage implements connector.TemplateBuilder.
//
// The platform verifies the certificate by default and the digest when one is
// given. Neither is decided here: the spec carries what the caller asked for,
// and a caller that asked for no digest has said so somewhere that was audited.
func (c *Connector) DownloadImage(ctx context.Context, spec connector.ImageDownloadSpec) (connector.TaskRef, error) {
	if spec.Node == "" || spec.Storage == "" || spec.URL == "" || spec.Filename == "" {
		return connector.TaskRef{}, connector.Errorf(connector.ErrInvalidConfig, "download_image",
			"downloading an image needs a node, a storage, a URL and a filename")
	}
	if (spec.Checksum == "") != (spec.ChecksumAlgorithm == "") {
		return connector.TaskRef{}, connector.Errorf(connector.ErrInvalidConfig, "download_image",
			"a checksum needs both a digest and an algorithm")
	}

	form := url.Values{}
	form.Set("content", importContent)
	form.Set("url", spec.URL)
	form.Set("filename", spec.Filename)
	if spec.Checksum != "" {
		form.Set("checksum", spec.Checksum)
		form.Set("checksum-algorithm", spec.ChecksumAlgorithm)
	}
	if spec.SkipTLSVerify {
		form.Set("verify-certificates", "0")
	}

	path := fmt.Sprintf("/nodes/%s/storage/%s/download-url", spec.Node, spec.Storage)
	var upid string
	if err := c.client.post(ctx, path, form, &upid); err != nil {
		return connector.TaskRef{}, err
	}
	return connector.TaskRef{ID: upid, Node: spec.Node}, nil
}

// CreateGuest implements connector.TemplateBuilder, making the shell the image
// will be imported into.
//
// The settings are the ones a cloud image actually needs, and no others. A
// serial console with `vga=serial0` matters most: cloud images log to serial
// and many ship without a framebuffer, so a template built without it produces
// guests whose console is a blank screen.
func (c *Connector) CreateGuest(ctx context.Context, spec connector.GuestCreateSpec) (connector.TaskRef, error) {
	if spec.Node == "" || spec.VMID == "" {
		return connector.TaskRef{}, connector.Errorf(connector.ErrInvalidConfig, "create_guest",
			"creating a guest needs a node and an identifier")
	}

	form := url.Values{}
	form.Set("vmid", spec.VMID)
	if spec.Name != "" {
		form.Set("name", spec.Name)
	}
	form.Set("cores", strconv.Itoa(orDefault(spec.Cores, 2)))
	form.Set("memory", strconv.Itoa(orDefault(spec.MemoryMB, 2048)))
	form.Set("ostype", "l26")
	// virtio-scsi-single is what Proxmox's own template guidance uses, and what
	// lets a disk be attached with iothread later without rewriting the
	// controller.
	form.Set("scsihw", "virtio-scsi-single")
	form.Set("agent", "1")
	form.Set("serial0", "socket")
	form.Set("vga", "serial0")
	if spec.Bridge != "" {
		form.Set("net0", "virtio,bridge="+spec.Bridge)
	}
	if spec.Description != "" {
		form.Set("description", spec.Description)
	}

	var upid string
	if err := c.client.post(ctx, "/nodes/"+spec.Node+"/qemu", form, &upid); err != nil {
		return connector.TaskRef{}, err
	}
	return connector.TaskRef{ID: upid, Node: spec.Node}, nil
}

// ImportDisk implements connector.TemplateBuilder.
//
// `import-from` is what removes the need to shell into a node: the platform
// converts the downloaded image into a disk on the target storage itself. This
// is the slow step — it copies and converts the whole image — which is why it
// is a task rather than a call that waits.
//
// The cloud-init drive is added in the same request. Two requests would leave a
// window in which the guest is a template-shaped thing with no way to take a
// user or an SSH key, and nothing would ever go back to fix it.
func (c *Connector) ImportDisk(ctx context.Context, vm connector.VMRef, spec connector.DiskImportSpec) (connector.TaskRef, error) {
	if vm.ExternalID == "" || vm.HostID == "" {
		return connector.TaskRef{}, connector.Errorf(connector.ErrInvalidConfig, "import_disk",
			"importing needs both the node and the guest id")
	}
	if spec.SourceVolume == "" || spec.Storage == "" {
		return connector.TaskRef{}, connector.Errorf(connector.ErrInvalidConfig, "import_disk",
			"importing needs a source volume and a storage to import it onto")
	}
	disk := spec.Disk
	if disk == "" {
		disk = "scsi0"
	}

	form := url.Values{}
	// "storage:0" means "allocate on this storage, sized from the source".
	form.Set(disk, fmt.Sprintf("%s:0,import-from=%s", spec.Storage, spec.SourceVolume))
	if spec.CloudInitDrive != "" {
		form.Set(spec.CloudInitDrive, spec.Storage+":cloudinit")
	}
	form.Set("boot", "order="+disk)

	guestType := vm.Type
	if guestType == "" {
		guestType = "qemu"
	}
	path := fmt.Sprintf("/nodes/%s/%s/%s/config", vm.HostID, guestType, vm.ExternalID)
	var upid string
	if err := c.client.post(ctx, path, form, &upid); err != nil {
		return connector.TaskRef{}, err
	}
	// A config write that changed nothing slow answers with no task at all;
	// an import returns one. Both are success, and the caller polls only when
	// there is something to poll.
	return connector.TaskRef{ID: upid, Node: vm.HostID}, nil
}

// ConvertToTemplate implements connector.TemplateBuilder.
//
// After this the guest cannot be started or edited — which is the point, and
// also why it is last: everything else must already be right.
func (c *Connector) ConvertToTemplate(ctx context.Context, vm connector.VMRef) (connector.TaskRef, error) {
	if vm.ExternalID == "" || vm.HostID == "" {
		return connector.TaskRef{}, connector.Errorf(connector.ErrInvalidConfig, "convert_template",
			"converting needs both the node and the guest id")
	}
	guestType := vm.Type
	if guestType == "" {
		guestType = "qemu"
	}
	path := fmt.Sprintf("/nodes/%s/%s/%s/template", vm.HostID, guestType, vm.ExternalID)
	var upid string
	if err := c.client.post(ctx, path, url.Values{}, &upid); err != nil {
		return connector.TaskRef{}, err
	}
	return connector.TaskRef{ID: upid, Node: vm.HostID}, nil
}

func orDefault(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}
