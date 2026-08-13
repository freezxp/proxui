// Package access is the Access bounded context: the grouping and grant model
// that decides which VMs a user can see. Roles say what a user may do; grants
// say what they may do it to (docs/12-domain-model.md).
package access

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidName is returned for empty or overlong group names.
var ErrInvalidName = errors.New("access: invalid group name")

// MaxNameLength bounds group names so UI tables and audit entries stay sane.
const MaxNameLength = 64

// UserGroup collects users who share access.
type UserGroup struct {
	ID          uuid.UUID
	Name        string
	Description string
	MemberCount int
	CreatedAt   time.Time
}

// VMGroup collects VMs that are granted together.
type VMGroup struct {
	ID          uuid.UUID
	Name        string
	Description string
	AutoRule    []byte // raw JSON; interpreted by the sync engine (RBAC-06)
	MemberCount int
	CreatedAt   time.Time
}

// Grant links a user group to a VM group. The pair is unique: granting twice
// is idempotent rather than additive.
type Grant struct {
	ID            uuid.UUID
	UserGroupID   uuid.UUID
	UserGroupName string
	VMGroupID     uuid.UUID
	VMGroupName   string
	GrantedBy     *uuid.UUID
	CreatedAt     time.Time
}

// ValidateName enforces the shared naming rule for both group kinds.
func ValidateName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ErrInvalidName
	}
	if len([]rune(trimmed)) > MaxNameLength {
		return ErrInvalidName
	}
	return nil
}
