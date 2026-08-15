import type { Role } from '@/api/types'

/**
 * Mirrors the server's permission map (internal/transport/http/permissions.go).
 *
 * This decides what the UI offers, never what it may do: the server checks
 * every request regardless. Hiding a button the user cannot use is courtesy;
 * the enforcement is elsewhere.
 */
export const can = {
  /** A brand-new account reaches nothing but its welcome page. Every other
   *  capability below is written to exclude it, and the server refuses it on
   *  every route regardless — this only decides what is worth showing. */
  useThePortal: (role: Role) => role !== 'newuser',
  viewInventory: (role: Role) => role !== 'newuser',
  /** Hosts, storage and networks describe the estate rather than the VMs in
   *  it. An operator works on what they were granted; surveying the nodes
   *  behind it is an administrator's job. */
  viewInfrastructure: (role: Role) => role === 'admin' || role === 'readonly' || role === 'auditor',
  openConsole: (role: Role) => role === 'admin' || role === 'operator',
  powerActions: (role: Role) => role === 'admin' || role === 'operator',
  editAnnotations: (role: Role) => role === 'admin' || role === 'operator',
  viewAudit: (role: Role) => role === 'admin' || role === 'auditor',
  managePlatforms: (role: Role) => role === 'admin',
  /**
   * Publishing puts a service on the public internet, which is a statement
   * about the network's boundary rather than about one machine. An operator's
   * grant over a VM must not imply it (ADR 0004, PUB-40).
   */
  publishApps: (role: Role) => role === 'admin',
  manageUsers: (role: Role) => role === 'admin',
  viewConsoleSessions: (role: Role) => role === 'admin',
}

export function roleLabel(role: Role): string {
  return {
    admin: 'Administrator',
    operator: 'Operator',
    readonly: 'Read only',
    auditor: 'Auditor',
    newuser: 'New user',
  }[role]
}
