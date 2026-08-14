// Package access is the Access bounded context: the grouping and grant model
// that decides which VMs a user can see. Roles say what a user may do; grants
// say what they may do it to (docs/12-domain-model.md).
package access

import (
	"encoding/json"
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
//
// The JSON tags are part of the API contract: without them these serialize as
// Go field names, which is neither the snake_case the rest of the API uses nor
// anything a client should have to guess at.
type UserGroup struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	MemberCount int       `json:"member_count"`
	CreatedAt   time.Time `json:"created_at"`
}

// VMGroup collects VMs that are granted together.
type VMGroup struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	// RawMessage rather than []byte: the rule is JSON, and a byte slice would
	// reach the client base64-encoded (RBAC-06).
	AutoRule    json.RawMessage `json:"auto_rule,omitempty"`
	MemberCount int             `json:"member_count"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Grant links a user group to a VM group. The pair is unique: granting twice
// is idempotent rather than additive.
type Grant struct {
	ID            uuid.UUID  `json:"id"`
	UserGroupID   uuid.UUID  `json:"user_group_id"`
	UserGroupName string     `json:"user_group_name"`
	VMGroupID     uuid.UUID  `json:"vm_group_id"`
	VMGroupName   string     `json:"vm_group_name"`
	GrantedBy     *uuid.UUID `json:"granted_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
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
