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
	// So does the event stream, for the same reason: a browser cannot put an
	// Authorization header on a WebSocket. The ticket names the user whose
	// events the socket will carry, so it is also the scoping.
	"GET /ws/events/{ticketID}": {Access: AccessPublic},
	// And the SSH terminal. Its ticket is stronger than the other two: it
	// names a connection that is already open and authenticated, so it is
	// single-use, sixty seconds long, and held only in this process's memory
	// (SSH-05).
	"GET /ws/ssh/{ticketID}": {Access: AccessPublic},
	"GET /readyz":            {Access: AccessPublic},

	// Branding is public because the sign-in page renders before anyone has
	// signed in. It exposes only what every visitor is meant to see: the
	// portal's name, its logo and any notice the administrator set.
	"GET /api/v1/branding": {Access: AccessPublic},
	// Registration and external sign-in are reachable without an account by
	// definition; whether they do anything is decided by policy, not by role.
	"GET /api/v1/auth/methods":         {Access: AccessPublic},
	"POST /api/v1/auth/register":       {Access: AccessPublic},
	"GET /api/v1/auth/google/start":    {Access: AccessPublic},
	"GET /api/v1/auth/google/callback": {Access: AccessPublic},
	"POST /api/v1/auth/login":          {Access: AccessPublic},
	// The second half of a login. Public for the same reason login is: the
	// caller has proved a password but has no session yet, and the challenge
	// id plus a code from the enrolled device is the whole credential.
	"POST /api/v1/auth/mfa":     {Access: AccessPublic},
	"POST /api/v1/auth/refresh": {Access: AccessPublic},
	"POST /api/v1/auth/logout":  {Access: AccessPublic},

	"GET /api/v1/auth/me":          {Access: AccessAuthenticated},
	"POST /api/v1/auth/logout-all": {Access: AccessAuthenticated},

	// Every role including a brand-new account: changing your own password
	// requires the current one, and an account that cannot do it is one an
	// administrator has to be involved in every time.
	"POST /api/v1/auth/password": roles(identity.RoleAdmin, identity.RoleOperator,
		identity.RoleReadOnly, identity.RoleAuditor, identity.RoleNewUser),

	// Enrolling a second factor is every role's business, including a
	// brand-new account: the account being changed is the caller's own, and
	// one that cannot add a factor without an administrator is one that
	// mostly will not have one (AUTH-04).
	"POST /api/v1/auth/me/totp": roles(identity.RoleAdmin, identity.RoleOperator,
		identity.RoleReadOnly, identity.RoleAuditor, identity.RoleNewUser),
	"POST /api/v1/auth/me/totp/confirm": roles(identity.RoleAdmin, identity.RoleOperator,
		identity.RoleReadOnly, identity.RoleAuditor, identity.RoleNewUser),
	// Removing one needs the account's password, checked in the command.
	"DELETE /api/v1/auth/me/totp": roles(identity.RoleAdmin, identity.RoleOperator,
		identity.RoleReadOnly, identity.RoleAuditor, identity.RoleNewUser),

	"GET /api/v1/users":                    roles(identity.RoleAdmin),
	"POST /api/v1/users":                   roles(identity.RoleAdmin),
	"GET /api/v1/users/{userID}":           roles(identity.RoleAdmin),
	"PUT /api/v1/users/{userID}":           roles(identity.RoleAdmin),
	"DELETE /api/v1/users/{userID}":        roles(identity.RoleAdmin),
	"POST /api/v1/users/{userID}/password": roles(identity.RoleAdmin),
	"DELETE /api/v1/users/{userID}/totp":   roles(identity.RoleAdmin),
	"PUT /api/v1/users/{userID}/groups":    roles(identity.RoleAdmin),

	"GET /api/v1/user-groups":                      roles(identity.RoleAdmin),
	"POST /api/v1/user-groups":                     roles(identity.RoleAdmin),
	"DELETE /api/v1/user-groups/{groupID}":         roles(identity.RoleAdmin),
	"GET /api/v1/vm-groups":                        roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"POST /api/v1/vm-groups":                       roles(identity.RoleAdmin),
	"GET /api/v1/vm-groups/{groupID}/members":      roles(identity.RoleAdmin),
	"PUT /api/v1/vm-groups/{groupID}/members":      roles(identity.RoleAdmin),
	"DELETE /api/v1/vm-groups/{groupID}":           roles(identity.RoleAdmin),
	"GET /api/v1/platforms":                        roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"POST /api/v1/platforms":                       roles(identity.RoleAdmin),
	"POST /api/v1/platforms/test":                  roles(identity.RoleAdmin),
	"GET /api/v1/platforms/{platformID}":           roles(identity.RoleAdmin),
	"PUT /api/v1/platforms/{platformID}":           roles(identity.RoleAdmin),
	"DELETE /api/v1/platforms/{platformID}":        roles(identity.RoleAdmin),
	"POST /api/v1/platforms/{platformID}/sync":     roles(identity.RoleAdmin),
	"GET /api/v1/platforms/{platformID}/sync-runs": roles(identity.RoleAdmin),

	// Provisioning (ADR 0010). Every one of these is admin-only: the platform
	// token can now create and destroy guests, so the role gate is doing work
	// that the credential's own limits used to do for free.
	"GET /api/v1/platforms/{platformID}/templates":  roles(identity.RoleAdmin),
	"POST /api/v1/platforms/{platformID}/provision": roles(identity.RoleAdmin),
	"POST /api/v1/platforms/{platformID}/templates": roles(identity.RoleAdmin),
	// Node prerequisites (ADR 0011). Installing changes software on a
	// hypervisor, which is the largest thing the portal does to a node.
	"GET /api/v1/platforms/{platformID}/readiness":             roles(identity.RoleAdmin),
	"POST /api/v1/platforms/{platformID}/nodes/{node}/install": roles(identity.RoleAdmin),
	"GET /api/v1/image-catalogue":                              roles(identity.RoleAdmin),

	// A user's own view of the inventory (INV-16…INV-19). Authenticated rather
	// than role-gated: starring a machine you can already see changes nothing
	// but your own list. Which machines those are is enforced per VM in the
	// command, which the role gate could not express.
	"PUT /api/v1/vms/{vmID}/favourite":           {Access: AccessAuthenticated},
	"DELETE /api/v1/vms/{vmID}/favourite":        {Access: AccessAuthenticated},
	"PUT /api/v1/vms/{vmID}/folder":              {Access: AccessAuthenticated},
	"GET /api/v1/folders":                        {Access: AccessAuthenticated},
	"POST /api/v1/folders":                       {Access: AccessAuthenticated},
	"PATCH /api/v1/folders/{folderID}":           {Access: AccessAuthenticated},
	"DELETE /api/v1/folders/{folderID}":          {Access: AccessAuthenticated},
	"PUT /api/v1/folders/{folderID}/vms":         {Access: AccessAuthenticated},
	"DELETE /api/v1/vms/{vmID}":                  roles(identity.RoleAdmin),
	"GET /api/v1/provision-requests":             roles(identity.RoleAdmin),
	"GET /api/v1/provision-requests/{requestID}": roles(identity.RoleAdmin),
	// Edge providers (ADR 0004). Admin only without exception: publishing an
	// app puts something on the public internet, which is a statement about
	// the network's boundary rather than about one machine. An operator's
	// grant over a VM must not imply it.
	"GET /api/v1/edge-providers":                        roles(identity.RoleAdmin),
	"POST /api/v1/edge-providers":                       roles(identity.RoleAdmin),
	"POST /api/v1/edge-providers/test":                  roles(identity.RoleAdmin),
	"POST /api/v1/edge-providers/{providerID}/verify":   roles(identity.RoleAdmin),
	"GET /api/v1/edge-providers/{providerID}/tunnels":   roles(identity.RoleAdmin),
	"GET /api/v1/edge-providers/{providerID}/ingress":   roles(identity.RoleAdmin),
	"POST /api/v1/edge-providers/{providerID}/snapshot": roles(identity.RoleAdmin),
	"POST /api/v1/edge-providers/{providerID}/preview":  roles(identity.RoleAdmin),
	"GET /api/v1/edge-providers/{providerID}/apps":      roles(identity.RoleAdmin),
	"POST /api/v1/edge-providers/{providerID}/apps":     roles(identity.RoleAdmin),
	"DELETE /api/v1/published-apps/{appID}":             roles(identity.RoleAdmin),
	"DELETE /api/v1/edge-providers/{providerID}":        roles(identity.RoleAdmin),

	"GET /api/v1/connectors":          roles(identity.RoleAdmin),
	"GET /api/v1/dashboard":           roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/vms":                 roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/vms/{vmID}":          roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/vms/{vmID}/metrics":  roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/vms/{vmID}/history":  roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"POST /api/v1/vms/{vmID}/console": roles(identity.RoleAdmin, identity.RoleOperator),
	"POST /api/v1/vms/{vmID}/power":   roles(identity.RoleAdmin, identity.RoleOperator),
	// The stream itself is authenticated by its ticket, at /ws/events/{id},
	// outside the API's role gates — the same shape as the console.
	"POST /api/v1/events/ticket":   roles(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/system/info":      roles(identity.RoleAdmin),
	"GET /api/v1/console-sessions": roles(identity.RoleAdmin),

	// SSH (SSH-01, SSH-09, SSH-10). The same role gate as the console, because
	// it is the same power over the same machines by a different door. What
	// the gate cannot express is that every file route also has to belong to
	// the caller — a signed-in operator holding someone else's session id must
	// get nothing — so the registry checks ownership on each call and the RBAC
	// matrix test only proves the outer fence.
	"POST /api/v1/vms/{vmID}/ssh":            roles(identity.RoleAdmin, identity.RoleOperator),
	"DELETE /api/v1/vms/{vmID}/ssh-host-key": roles(identity.RoleAdmin),
	"GET /api/v1/ssh-sessions":               roles(identity.RoleAdmin),
	"DELETE /api/v1/ssh-sessions/{sessionID}": roles(identity.RoleAdmin,
		identity.RoleOperator),
	"GET /api/v1/ssh-sessions/{sessionID}/files": roles(identity.RoleAdmin,
		identity.RoleOperator),
	"DELETE /api/v1/ssh-sessions/{sessionID}/files": roles(identity.RoleAdmin,
		identity.RoleOperator),
	"GET /api/v1/ssh-sessions/{sessionID}/files/content": roles(identity.RoleAdmin,
		identity.RoleOperator),
	"POST /api/v1/ssh-sessions/{sessionID}/files/content": roles(identity.RoleAdmin,
		identity.RoleOperator),
	"POST /api/v1/ssh-sessions/{sessionID}/files/mkdir": roles(identity.RoleAdmin,
		identity.RoleOperator),
	"POST /api/v1/ssh-sessions/{sessionID}/files/rename": roles(identity.RoleAdmin,
		identity.RoleOperator),
	"POST /api/v1/ssh-sessions/{sessionID}/files/chmod": roles(identity.RoleAdmin,
		identity.RoleOperator),

	// The portal's own SSH key (SSH-11..SSH-14, ADR 0006).
	//
	// Reading the public half is an operator's business: they are the ones who
	// paste it into a cloud-init template, and it is public by construction.
	// Holding the pair — generating, rotating, destroying it — is an
	// administrator's, because a rotation silently invalidates every install
	// in the estate and the list of those installs is a map of where the key
	// opens a door.
	//
	// Installing and removing it run over an SSH session the caller already
	// authenticated: the registry refuses a session that is not theirs, so the
	// grant being exercised is one they already had.
	"GET /api/v1/ssh-key": roles(identity.RoleAdmin, identity.RoleOperator),
	"GET /api/v1/vms/{vmID}/ssh-key": roles(identity.RoleAdmin,
		identity.RoleOperator),
	"POST /api/v1/ssh-key":         roles(identity.RoleAdmin),
	"DELETE /api/v1/ssh-key":       roles(identity.RoleAdmin),
	"GET /api/v1/ssh-key/installs": roles(identity.RoleAdmin),
	"POST /api/v1/ssh-sessions/{sessionID}/portal-key": roles(identity.RoleAdmin,
		identity.RoleOperator),
	"DELETE /api/v1/ssh-sessions/{sessionID}/portal-key": roles(identity.RoleAdmin,
		identity.RoleOperator),
	"PUT /api/v1/vms/{vmID}/tags":  roles(identity.RoleAdmin, identity.RoleOperator),
	"PUT /api/v1/vms/{vmID}/notes": roles(identity.RoleAdmin, identity.RoleOperator),

	"GET /api/v1/audit-logs":            roles(identity.RoleAdmin, identity.RoleAuditor),
	"GET /api/v1/audit-logs/export":     roles(identity.RoleAdmin, identity.RoleAuditor),
	"GET /api/v1/audit-logs/categories": roles(identity.RoleAdmin, identity.RoleAuditor),

	"GET /api/v1/notification-channels":                   roles(identity.RoleAdmin),
	"POST /api/v1/notification-channels":                  roles(identity.RoleAdmin),
	"PUT /api/v1/notification-channels/{channelID}":       roles(identity.RoleAdmin),
	"DELETE /api/v1/notification-channels/{channelID}":    roles(identity.RoleAdmin),
	"POST /api/v1/notification-channels/{channelID}/test": roles(identity.RoleAdmin),
	"GET /api/v1/notification-rules":                      roles(identity.RoleAdmin),
	"POST /api/v1/notification-rules":                     roles(identity.RoleAdmin),
	"DELETE /api/v1/notification-rules/{ruleID}":          roles(identity.RoleAdmin),
	"GET /api/v1/notification-deliveries":                 roles(identity.RoleAdmin),

	// Deliberately without RoleOperator: an operator sees the VMs they were
	// granted, not the estate those VMs sit in.
	"GET /api/v1/hosts":                          roles(identity.RoleAdmin, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/hosts/{hostID}/metrics":         roles(identity.RoleAdmin, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/hosts/{hostID}/sensors":         roles(identity.RoleAdmin, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/hosts/{hostID}/sensors/series":  roles(identity.RoleAdmin, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/hosts/{hostID}/sensors/history": roles(identity.RoleAdmin, identity.RoleReadOnly, identity.RoleAuditor),
	// Clearing a pin is what lets a changed node be trusted again, so it is
	// an administrator's decision and an audited one.
	"DELETE /api/v1/hosts/{hostID}/host-key": roles(identity.RoleAdmin),
	"GET /api/v1/storage":                    roles(identity.RoleAdmin, identity.RoleReadOnly, identity.RoleAuditor),
	"GET /api/v1/networks":                   roles(identity.RoleAdmin, identity.RoleReadOnly, identity.RoleAuditor),

	"GET /api/v1/settings":       roles(identity.RoleAdmin),
	"PUT /api/v1/settings/{key}": roles(identity.RoleAdmin),

	"GET /api/v1/alert-rules":             roles(identity.RoleAdmin),
	"POST /api/v1/alert-rules":            roles(identity.RoleAdmin),
	"PUT /api/v1/alert-rules/{ruleID}":    roles(identity.RoleAdmin),
	"DELETE /api/v1/alert-rules/{ruleID}": roles(identity.RoleAdmin),
	"GET /api/v1/alerts": roles(identity.RoleAdmin, identity.RoleOperator,
		identity.RoleReadOnly, identity.RoleAuditor),

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
