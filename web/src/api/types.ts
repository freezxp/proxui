/** Shapes returned by the Go API. Hand-written for now; generated from
 *  OpenAPI once the specification is committed (docs/23 recipe). */

export type Role = 'admin' | 'operator' | 'readonly' | 'auditor' | 'newuser'

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
  role: Role
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

export interface AuditEntry {
  id: number
  ts: string
  actor_user_id?: string
  actor_name: string
  category: string
  action: string
  target_type?: string
  target_id?: string
  target_name?: string
  source_ip?: string
  user_agent?: string
  outcome: string
  request_id?: string
  details?: Record<string, unknown>
}

export interface NotificationChannel {
  id: string
  name: string
  kind: 'email' | 'slack' | 'webhook'
  config: Record<string, unknown>
  is_enabled: boolean
  has_secret: boolean
  created_at: string
}

export interface NotificationRule {
  id: string
  category: string
  min_severity: string
  platform_id?: string
  vm_group_id?: string
  channel_id: string
  channel_name: string
  is_enabled: boolean
  created_at: string
}

export interface Delivery {
  id: number
  channel_id: string
  channel_name: string
  subject: string
  status: string
  attempts: number
  last_error?: string
  created_at: string
  sent_at?: string
}

export interface AlertRule {
  id: string
  name: string
  metric: string
  op: string
  threshold: number
  duration_s: number
  vm_group_id?: string
  severity: string
  cooldown_s: number
  is_enabled: boolean
  firing_count: number
  created_at: string
}

export interface AlertStatus {
  rule_id: string
  rule_name: string
  vm_id: string
  vm_name: string
  metric: string
  severity: string
  state: string
  since: string
  last_value: number
  last_notified_at?: string
}

export interface HostRow {
  id: string
  name: string
  platform_name: string
  status: string
  cpu_cores: number
  memory_bytes: number
  version: string
  uptime_s: number
  sync_state: string
  vm_count: number
}

export interface StorageRow {
  id: string
  name: string
  platform_name: string
  host_name?: string
  storage_type: string
  total_bytes: number
  used_bytes: number
  is_shared: boolean
  sync_state: string
}

export interface NetworkRow {
  id: string
  name: string
  platform_name: string
  host_name?: string
  net_type: string
  cidr: string
  vlan_tag?: number
  sync_state: string
}

export interface Setting {
  key: string
  group: string
  label: string
  help: string
  kind: 'duration_s' | 'count' | 'days' | 'text' | 'image' | 'select' | 'secret'
  default?: number
  min?: number
  max?: number
  default_text?: string
  max_length?: number
  value?: number
  text?: string
  has_value?: boolean
  options?: { value: string; label: string }[]
  modified: boolean
}

// --- edge providers and published apps (docs/28) -------------------------

export interface EdgeProvider {
  id: string
  name: string
  kind: string
  account_id: string
  tunnel_id: string
  tunnel_name: string
  allowed_zone_ids: string[]
  is_enabled: boolean
  /** A credential that works but has no tunnel chosen is a real state. */
  ready: boolean
  health: 'unknown' | 'healthy' | 'degraded' | 'unreachable'
  health_detail: string
  last_seen_at: string | null
  created_at: string
}

export interface EdgeTunnel {
  id: string
  name: string
  remotely_managed: boolean
  connections: number
  /** False for a locally-managed or deleted tunnel: the API cannot change it. */
  manageable: boolean
  active: boolean
  /** Why a tunnel is unusable, or why it is idle. */
  reason?: string
}

export interface ScopeGap {
  scope: string
  blocks: string
}

export interface EdgeHealth {
  reachable: boolean
  authenticated: boolean
  missing_scopes: ScopeGap[]
  tunnels: EdgeTunnel[]
  warnings: string[]
}

export type RuleOrigin = 'portal' | 'external' | 'catch_all'

export interface IngressRule {
  index: number
  hostname: string
  path?: string
  service: string
  origin: RuleOrigin
  /** Serves this portal; nothing may remove or shadow it. */
  is_portal: boolean
  is_catch_all: boolean
  /** Points at an address no known VM holds — a VM that moved or was deleted. */
  unmatched: boolean
  target_host?: string
  target_port?: number
  vm?: { id: string; name: string; state: VMState }
}

export interface IngressView {
  provider_id: string
  tunnel_id: string
  tunnel_name: string
  /** Carried into a write so a concurrent change is refused, not clobbered. */
  version: number
  rules: IngressRule[]
  portal_owned: number
  external: number
  unmatched: number
}

export type PreviewChange = 'added' | 'removed' | 'modified' | 'moved' | 'unchanged'

export interface PreviewEntry {
  change: PreviewChange
  hostname: string
  path?: string
  before?: string
  after?: string
  from_index: number
  to_index: number
}

export interface PreviewResult {
  safe: boolean
  refusal?: string
  stale: boolean
  stale_detail?: string
  current_version: number
  changes: boolean
  added: number
  removed: number
  modified: number
  moved: number
  unchanged: number
  entries: PreviewEntry[]
}

export interface PublishedApp {
  id: string
  provider_id: string
  hostname: string
  path?: string
  service_url: string
  vm_id?: string
  vm_port?: number
  is_enabled: boolean
  /** False for a DNS record the portal adopted rather than created. */
  manages_dns: boolean
  url: string
  last_applied_at?: string
  last_error?: string
  created_at: string
}
