package proxmox

import (
	"context"
	"fmt"
	"net/url"

	"github.com/freezxp/proxui/internal/connector"
)

// Power implements connector.PowerManager.
//
// Proxmox power calls are asynchronous: they return a UPID naming a task the
// caller can poll. The connector hands that reference back rather than waiting,
// so a slow shutdown does not block the request that triggered it.
func (c *Connector) Power(ctx context.Context, vm connector.VMRef, action connector.PowerAction) (connector.TaskRef, error) {
	if !action.Valid() {
		return connector.TaskRef{}, connector.Errorf(connector.ErrNotSupported, "power",
			"unknown power action %q", action)
	}
	if vm.HostID == "" || vm.ExternalID == "" {
		return connector.TaskRef{}, connector.Errorf(connector.ErrInvalidConfig, "power",
			"power actions need both the node and the VMID")
	}
	guestType := vm.Type
	if guestType == "" {
		guestType = "qemu"
	}

	path := fmt.Sprintf("/nodes/%s/%s/%s/status/%s", vm.HostID, guestType, vm.ExternalID, action)

	var upid string
	if err := c.client.post(ctx, path, url.Values{}, &upid); err != nil {
		return connector.TaskRef{}, err
	}
	return connector.TaskRef{ID: upid, Node: vm.HostID}, nil
}

// taskStatus is the state of an asynchronous Proxmox task.
type taskStatus struct {
	Status     string `json:"status"`     // running | stopped
	ExitStatus string `json:"exitstatus"` // OK, or an error string
	Type       string `json:"type"`
	Node       string `json:"node"`
}

// TaskState reports whether an asynchronous task finished and whether it
// succeeded. The sync engine polls this after a power action so the audit entry
// records the real outcome rather than merely that a request was accepted.
func (c *Connector) TaskState(ctx context.Context, task connector.TaskRef) (done bool, ok bool, detail string, err error) {
	if task.ID == "" || task.Node == "" {
		return false, false, "", connector.Errorf(connector.ErrInvalidConfig, "task", "task reference is incomplete")
	}
	path := fmt.Sprintf("/nodes/%s/tasks/%s/status", task.Node, task.ID)

	var status taskStatus
	if err := c.client.get(ctx, path, &status); err != nil {
		return false, false, "", err
	}
	if status.Status != "stopped" {
		return false, false, status.Status, nil
	}
	return true, status.ExitStatus == "OK", status.ExitStatus, nil
}
