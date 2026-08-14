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

export interface MetricPoint {
  t: string
  cpu_pct: number
  mem_used_bytes: number
  mem_total_bytes: number
  disk_read_bps: number
  disk_write_bps: number
  net_rx_bps: number
  net_tx_bps: number
  disk_used_bytes: number
}

export interface MetricSeriesResponse {
  vm_id: string
  from: string
  to: string
  series: {
    /** Which stored resolution answered: shown to the user so a chart never
     *  implies more precision than it has. */
    resolution: string
    bucket_seconds: number
    points: MetricPoint[]
  }
}

export interface HistoryEntry {
  changed_at: string
  field: string
  old_value: string
  new_value: string
}

export interface VMDetail extends VMListItem {
  notes: string
  groups: string[]
  attrs?: Record<string, unknown>
  first_seen_at: string
}

/** Connector-declared form description. The UI renders a platform form from
 *  this alone, so a new connector needs no frontend change. */
export interface SchemaField {
  key: string
  label: string
  kind: 'text' | 'number' | 'bool' | 'select' | 'secret'
  required?: boolean
  help?: string
  default?: unknown
  options?: { value: string; label: string }[]
  placeholder?: string
}

export interface CredentialForm {
  kind: string
  label: string
  help?: string
  fields: SchemaField[]
}

export interface ConnectorInfo {
  type: string
  display_name: string
  version: string
  schema: {
    endpoint_label?: string
    endpoint_help?: string
    fields?: SchemaField[]
    credentials?: CredentialForm[]
  }
}

export interface Platform {
  id: string
  name: string
  type: string
  endpoint_url: string
  datacenter: string
  is_enabled: boolean
  tls_mode: string
  health: string
  health_detail?: string
  detected_version?: string
  last_seen_at?: string
  sync_intervals: { inventory: number; metrics: number; health: number }
  breaker_open: boolean
  created_at: string
}

export interface TestReport {
  reachable: boolean
  authenticated: boolean
  version?: string
  nodes?: number
  missing_permissions?: string[]
  warnings?: string[]
  error?: string
}

export interface SyncRun {
  id: number
  kind: string
  status: string
  trigger: string
  started_at: string
  finished_at?: string
  duration_ms: number
  error?: string
  stats: Record<string, number>
}

export interface User {
  id: string
  username: string
  email: string
  display_name: string
  role: 'admin' | 'operator' | 'readonly' | 'auditor'
  is_active: boolean
  totp_enabled: boolean
  must_change_password: boolean
  last_login_at?: string
  created_at: string
}

export interface UserGroup {
  id: string
  name: string
  description: string
  member_count: number
  created_at: string
}

export interface VMGroup extends UserGroup {
  auto_rule?: unknown
}

export interface Grant {
  id: string
  user_group_id: string
  user_group_name: string
  vm_group_id: string
  vm_group_name: string
  granted_by?: string
  created_at: string
}
