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

/** A login that stopped at the password because a second factor is enrolled
 *  (AUTH-04). Not an error: the credentials were right, and the sign-in
 *  finishes at POST /auth/mfa with a code. */
export interface MFAChallengeResponse {
  mfa_required: true
  mfa_token: string
  expires_in: number
}

/** What enrolment hands back — the one moment the seed is readable, so it can
 *  reach an authenticator app. */
export interface TOTPEnrollment {
  secret: string
  otpauth_url: string
  digits: number
  period: number
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
  /** When the platform itself last confirmed this state. Absent means the row
   *  is as the last sync left it (docs/10 §10.6), which the UI says rather
   *  than implying a freshness it does not have. */
  live_at?: string
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
  /** Every address the platform answers on, configured first (ADR 0009). Detail responses only. */
  endpoints?: PlatformEndpoint[]
}

export interface PlatformEndpoint {
  address: string
  fingerprint?: string
  source: string
  refreshed_at: string
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
  platform_id: string
  /** Absent until the node has answered — which needs the portal's public key
   *  in its authorized_keys and lm-sensors installed (ADR 0007). */
  sensors?: SensorSummary
  /** Why the last poll got nothing, in the operator's terms. `sensors_ever_read`
   *  separates "never tried" from "tried and was refused". */
  sensor_error?: string
  sensors_ever_read: boolean
}

/** One sensor as the hardware reports it. `chip` and `label` are verbatim, so
 *  they match what `sensors` prints on the node itself. */
export interface SensorReading {
  chip: string
  label: string
  kind: 'temp_c' | 'fan_rpm'
  value: number
  /** The chip's own limits, absent on hardware that declares none. 80°C means
   *  something different on a package than on a VRM. */
  high?: number
  crit?: number
}

export interface SensorSummary {
  hottest: SensorReading
  count: number
  chips: string[]
}

/** How the portal reaches a node, and how the last poll went. */
export interface NodeSSH {
  address: string
  ssh_user: string
  algorithm: string
  fingerprint: string
  first_seen_at: string
  last_tried_at?: string
  last_ok_at?: string
  last_error?: string
}

export interface SensorSeries {
  chip: string
  label: string
  crit?: number
  points: { t: string; v: number; max?: number }[]
}

export interface NodeSensors {
  host_id: string
  at?: string
  readings: SensorReading[]
  summary: SensorSummary
  node?: NodeSSH
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

export interface EdgeZone {
  id: string
  name: string
}

export interface EdgeHealth {
  reachable: boolean
  authenticated: boolean
  missing_scopes: ScopeGap[]
  tunnels: EdgeTunnel[]
  /** Zones the credential can see; the write boundary is chosen from these. */
  zones: EdgeZone[]
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

// --- SSH terminal and file transfer (SSH-01..SSH-10) --------------------

export interface SshHostKey {
  algorithm: string
  fingerprint: string
}

/** What the server hands back once a connection is open and authenticated.
 *  It never echoes the credential; `ws_url` carries a ticket that is good for
 *  one attachment and sixty seconds. */
export interface SshSession {
  session_id: string
  ws_url: string
  expires_in: number
  address: string
  ssh_user: string
  host_key: SshHostKey
  home: string
  files_available: boolean
  files_detail?: string
}

/** The body of a 409 ssh.host_key_unknown — the fingerprint a human has to
 *  confirm before the portal will pin it. */
export interface SshHostKeyPrompt {
  address: string
  algorithm: string
  fingerprint: string
}

/** The body of a 409 ssh.host_key_mismatch. */
export interface SshHostKeyMismatch {
  address: string
  expected: string
  got: string
  first_seen_at: string
}

export interface RemoteFile {
  name: string
  path: string
  size: number
  mode: string
  mode_bits: number
  is_dir: boolean
  is_link: boolean
  target?: string
  owner?: string
  group?: string
  mod_time: string
}

export interface RemoteListing {
  path: string
  /** Empty at the filesystem root, which is where "up" stops. */
  parent: string
  data: RemoteFile[]
}

// --- the portal's own SSH key (SSH-11..SSH-14, ADR 0006) ----------------

/** The portal's key pair, as the API describes it. Only ever the public half:
 *  the private one lives in the vault and is used server-side to dial. */
export interface PortalKey {
  exists: boolean
  /** A complete authorized_keys line, ready to paste into cloud-init. */
  public_key?: string
  algorithm?: string
  fingerprint?: string
  created_at?: string
}

/** One account on one guest whose authorized_keys the portal believes carries
 *  the key. `stale` means it carries an older key — a rotation left it behind,
 *  and it will not authenticate. */
export interface PortalKeyInstall {
  vm_id: string
  vm_name?: string
  ssh_user: string
  fingerprint: string
  installed_at: string
  installed_by: string
  stale: boolean
}

/** What the connect form asks before offering key auth for a VM. */
export interface VMPortalKeyState {
  data: PortalKeyInstall[]
  key_exists: boolean
  fingerprint?: string
}

export interface SshSessionRecord {
  id: string
  user_id: string
  username: string
  vm_id: string
  vm_name: string
  ssh_user: string
  address: string
  client_ip?: string
  started_at: string
  connected_at?: string
  ended_at?: string
  close_reason?: string
  bytes_tx: number
  bytes_rx: number
  active: boolean
}
