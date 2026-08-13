package httpapi

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/freezxp/proxui/internal/domain/identity"
)

// Access describes who may call a route.
type Access int

const (
	// AccessPublic requires no authentication (login, health probes).
	AccessPublic Access = iota
	// AccessAuthenticated requires any valid session, no particular role.
	AccessAuthenticated
	// AccessRoles requires one of the listed roles.
	AccessRoles
)

// Permission declares the authorization requirement for one route.
type Permission struct {
	Access Access
	Roles  []identity.Role
}

// Allows reports whether role may call the route. Callers must handle
// AccessPublic separately: it needs no principal at all.
func (p Permission) Allows(role identity.Role) bool {
	switch p.Access {
	case AccessPublic, AccessAuthenticated:
		return true
	case AccessRoles:
		for _, r := range p.Roles {
			if r == role {
				return true
			}
		}
	}
	return false
}

func roles(rs ...identity.Role) Permission { return Permission{Access: AccessRoles, Roles: rs} }

// permissionMap is the single declaration of who may call what. It is the
// source of truth for three things: the boot check that no route ships
// undeclared, the generated RBAC matrix test, and code review. Adding a route
// without adding an entry here fails startup (deny by default, RBAC-07).
var permissionMap = map[string]Permission{
	"GET /healthz": {Access: AccessPublic},
	// The console socket carries its own single-use ticket; the permission
	// check happened when that ticket was issued.
	"GET /ws/console/{ticketID}": {Access: AccessPublic},
	"GET /readyz":                {Access: AccessPublic},

	"POST /api/v1/auth/login":   {Access: AccessPublic},
	"POST /api/v1/auth/refresh": {Access: AccessPublic},
	"POST /api/v1/auth/logout":  {Access: AccessPublic},

	"GET /api/v1/auth/me":          {Access: AccessAuthenticated},
	"POST /api/v1/auth/logout-all": {Access: AccessAuthenticated},

	"GET /api/v1/users":                    roles(identity.RoleAdmin),
	"POST /api/v1/users":                   roles(identity.RoleAdmin),
	"GET /api/v1/users/{userID}":           roles(identity.RoleAdmin),
	"PUT /api/v1/users/{userID}":           roles(identity.RoleAdmin),
	"POST /api/v1/users/{userID}/password": roles(identity.RoleAdmin),
	"PUT /api/v1/users/{userID}/groups":    roles(identity.RoleAdmin),

	"GET /api/v1/user-groups":                      roles(identity.RoleAdmin),
	"POST /api/v1/user-groups":                     roles(identity.RoleAdmin),
	"DELETE /api/v1/user-groups/{groupID}":         roles(identity.RoleAdmin),
	"GET /api/v1/vm-groups":                        roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"POST /api/v1/vm-groups":                       roles(identity.RoleAdmin),
	"DELETE /api/v1/vm-groups/{groupID}":           roles(identity.RoleAdmin),
	"GET /api/v1/platforms":                        roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"POST /api/v1/platforms":                       roles(identity.RoleAdmin),
	"POST /api/v1/platforms/test":                  roles(identity.RoleAdmin),
	"GET /api/v1/platforms/{platformID}":           roles(identity.RoleAdmin),
	"PUT /api/v1/platforms/{platformID}":           roles(identity.RoleAdmin),
	"DELETE /api/v1/platforms/{platformID}":        roles(identity.RoleAdmin),
	"POST /api/v1/platforms/{platformID}/sync":     roles(identity.RoleAdmin),
	"GET /api/v1/platforms/{platformID}/sync-runs": roles(identity.RoleAdmin),
	"GET /api/v1/connectors":                       roles(identity.RoleAdmin),
	"GET /api/v1/dashboard":                        roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/vms":                              roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/vms/{vmID}":                       roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/vms/{vmID}/metrics":               roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/vms/{vmID}/history":               roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"POST /api/v1/vms/{vmID}/console":              roles(identity.RoleAdmin, identity.RoleOperator),
	"GET /api/v1/console-sessions":                 roles(identity.RoleAdmin),
	"PUT /api/v1/vms/{vmID}/tags":                  roles(identity.RoleAdmin, identity.RoleOperator),
	"PUT /api/v1/vms/{vmID}/notes":                 roles(identity.RoleAdmin, identity.RoleOperator),

	"GET /api/v1/audit-logs":            roles(identity.RoleAdmin, identity.RoleAuditor),
	"GET /api/v1/audit-logs/export":     roles(identity.RoleAdmin, identity.RoleAuditor),
	"GET /api/v1/audit-logs/categories": roles(identity.RoleAdmin, identity.RoleAuditor),

	"GET /api/v1/grants":              roles(identity.RoleAdmin),
	"POST /api/v1/grants":             roles(identity.RoleAdmin),
	"DELETE /api/v1/grants/{grantID}": roles(identity.RoleAdmin),
}

// PermissionFor returns the declared permission for a method and route pattern.
func PermissionFor(method, pattern string) (Permission, bool) {
	p, ok := permissionMap[routeKey(method, pattern)]
	return p, ok
}

// PermissionRoutes returns every declared route key, sorted. The RBAC matrix
// test iterates this so new routes automatically gain denial coverage.
func PermissionRoutes() []string {
	keys := make([]string, 0, len(permissionMap))
	for k := range permissionMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func routeKey(method, pattern string) string {
	return method + " " + strings.TrimSuffix(pattern, "/")
}

// ValidatePermissions walks the wired route tree and fails if any route lacks a
// permission-map entry, or the map declares a route that does not exist. Called
// at boot so an unprotected endpoint can never reach production.
func ValidatePermissions(router chi.Routes) error {
	wired := map[string]bool{}
	var undeclared []string

	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := routeKey(method, route)
		wired[key] = true
		if _, ok := permissionMap[key]; !ok {
			undeclared = append(undeclared, key)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk routes: %w", err)
	}

	var stale []string
	for key := range permissionMap {
		if !wired[key] {
			stale = append(stale, key)
		}
	}

	if len(undeclared) > 0 || len(stale) > 0 {
		sort.Strings(undeclared)
		sort.Strings(stale)
		return fmt.Errorf("permission map out of sync: undeclared routes %v; declared but not wired %v", undeclared, stale)
	}
	return nil
}
