/** Shapes returned by the Go API. Hand-written for now; generated from
 *  OpenAPI once the specification is committed (docs/23 recipe). */

export type Role = 'admin' | 'operator' | 'readonly' | 'auditor'

export interface CurrentUser {
  id: string
  username: string
  email: string
  display_name: string
  role: Role
  totp_enabled: boolean
  must_change_password: boolean
}

export interface TokenResponse {
  access_token: string
  token_type: string
  expires_in: number
}

export interface PlatformHealth {
  id: string
  name: string
  datacenter: string
  health: 'unknown' | 'healthy' | 'degraded' | 'unreachable'
  version?: string
  last_seen_at: string
  breaker_open: boolean
  vm_count: number
}

export interface TopConsumer {
  vm_id: string
  name: string
  platform_name: string
  value: number
}

export interface Dashboard {
  total_vms: number
  running_vms: number
  stopped_vms: number
  other_vms: number
  missing_vms: number
  platforms: PlatformHealth[]
  top_cpu: TopConsumer[]
  top_memory: TopConsumer[]
}

export type VMState = 'running' | 'stopped' | 'paused' | 'suspended' | 'unknown'

export interface VMListItem {
  id: string
  external_id: string
  name: string
  vm_type: string
  state: VMState
  cpu_cores: number
  memory_bytes: number
  disk_bytes: number
  uptime_s: number
  ip_addresses: string[]
  platform_tags: string[]
  portal_tags: string[]
  sync_state: 'active' | 'missing' | 'deleted'
  last_seen_at: string
  platform_id: string
  platform_name: string
  datacenter: string
  host_name?: string
  cpu_pct: number
  mem_pct: number
}

export interface Paged<T> {
  data: T[]
  meta: { total: number; page?: number; per_page?: number }
}
