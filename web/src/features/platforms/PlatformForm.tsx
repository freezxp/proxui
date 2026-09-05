import { useMemo, useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { api, ApiError } from '@/api/client'
import type { ConnectorInfo, Platform, TestReport } from '@/api/types'
import { Drawer } from '@/components/Drawer'
import { SchemaFields, defaultsFor, missingRequired, type Values } from './SchemaForm'

const TLS_MODES = [
  {
    value: 'verify',
    label: 'Verify with system roots',
    help: 'For a platform with a publicly trusted certificate.',
  },
  {
    value: 'fingerprint',
    label: 'Pin a SHA-256 fingerprint',
    help: 'The usual choice for a default Proxmox install, which is self-signed.',
  },
  {
    value: 'custom_ca',
    label: 'Verify with a supplied CA',
    help: 'For an internal certificate authority.',
  },
  {
    value: 'insecure',
    label: 'Do not verify',
    help: 'Accepts any certificate. Audited, and it means anyone on the network path can read this traffic.',
  },
]

export function PlatformForm({
  platform,
  connectors,
  onClose,
  onSaved,
}: {
  platform: Platform | null
  connectors: ConnectorInfo[]
  onClose: () => void
  onSaved: () => void
}) {
  const editing = platform !== null
  const [type, setType] = useState(platform?.type ?? connectors[0]?.type ?? '')
  const connector = connectors.find((c) => c.type === type)
  const schema = connector?.schema

  const [name, setName] = useState(platform?.name ?? '')
  const [endpoint, setEndpoint] = useState(platform?.endpoint_url ?? '')
  const [datacenter, setDatacenter] = useState(platform?.datacenter ?? '')
  const [tlsMode, setTLSMode] = useState(platform?.tls_mode ?? 'fingerprint')
  const [fingerprint, setFingerprint] = useState('')
  const [caPEM, setCAPEM] = useState('')
  const [config, setConfig] = useState<Values>(() => defaultsFor(schema?.fields))
  const [credentialKind, setCredentialKind] = useState(
    schema?.credentials?.[0]?.kind ?? 'api_token',
  )
  const [credentials, setCredentials] = useState<Values>({})
  const [report, setReport] = useState<TestReport | null>(null)
  const [error, setError] = useState('')

  const credentialForm = schema?.credentials?.find((c) => c.kind === credentialKind)

  // Any change to what would be sent invalidates a previous test result:
  // showing a green tick for a configuration that is no longer on screen
  // would be worse than showing nothing.
  function invalidate<T>(setter: (value: T) => void) {
    return (value: T) => {
      setReport(null)
      setter(value)
    }
  }

  const payload = useMemo(
    () => ({
      name,
      type,
      endpoint_url: endpoint,
      datacenter,
      tls_mode: tlsMode,
      tls_fingerprint: tlsMode === 'fingerprint' ? fingerprint : '',
      tls_ca_pem: tlsMode === 'custom_ca' ? caPEM : '',
      config,
      credential_kind: credentialKind,
      ...credentials,
    }),
    [
      name,
      type,
      endpoint,
      datacenter,
      tlsMode,
      fingerprint,
      caPEM,
      config,
      credentialKind,
      credentials,
    ],
  )

  const test = useMutation({
    mutationFn: () => api.post<TestReport>('/platforms/test', payload),
    onSuccess: (result) => {
      setReport(result)
      setError('')
    },
    onError: (err) => {
      // A failed probe is an answer, and the server returns the partial report
      // with it. Showing how far it got is the whole value of this button.
      if (err instanceof ApiError && err.body) setReport(err.body as TestReport)
      else setError(err instanceof Error ? err.message : 'The connection test could not run.')
    },
  })

  const save = useMutation({
    mutationFn: () =>
      editing
        ? api.put<Platform>(`/platforms/${platform.id}`, payload)
        : api.post<Platform>('/platforms', payload),
    onSuccess: onSaved,
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not save the platform.'),
  })

  const missing = [
    ...(!name.trim() ? ['Name'] : []),
    ...(!endpoint.trim() ? [schema?.endpoint_label ?? 'Endpoint'] : []),
    ...missingRequired(schema?.fields, config),
    // On edit the stored secret stands in for an empty box.
    ...(editing ? [] : missingRequired(credentialForm?.fields, credentials)),
  ]

  const tested = report?.reachable && report?.authenticated
  const canSave = missing.length === 0 && (tested || editing)

  return (
    <Drawer
      title={editing ? `Edit ${platform.name}` : 'Add platform'}
      onClose={onClose}
      footer={
        <div className="flex items-center justify-between gap-3">
          <button
            onClick={() => test.mutate()}
            disabled={test.isPending || missing.length > 0}
            className="rounded-md border border-border px-3 py-2 text-sm disabled:opacity-40"
          >
            {test.isPending ? 'Testing…' : 'Test connection'}
          </button>
          <div className="flex gap-2">
            <button onClick={onClose} className="rounded-md border border-border px-3 py-2 text-sm">
              Cancel
            </button>
            <button
              onClick={() => save.mutate()}
              disabled={!canSave || save.isPending}
              title={
                missing.length > 0
                  ? `Still needed: ${missing.join(', ')}`
                  : !tested && !editing
                    ? 'Test the connection before saving'
                    : undefined
              }
              className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-40"
            >
              {save.isPending ? 'Saving…' : editing ? 'Save changes' : 'Add platform'}
            </button>
          </div>
        </div>
      }
    >
      <div className="space-y-4">
        <div className="space-y-1">
          <label htmlFor="platform-type" className="block text-sm">
            Platform type
          </label>
          <select
            id="platform-type"
            value={type}
            disabled={editing} // changing type would invalidate every stored field
            onChange={(e) => {
              const next = e.target.value
              setType(next)
              const nextSchema = connectors.find((c) => c.type === next)?.schema
              setConfig(defaultsFor(nextSchema?.fields))
              setCredentialKind(nextSchema?.credentials?.[0]?.kind ?? 'api_token')
              setCredentials({})
              setReport(null)
            }}
            className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm disabled:opacity-60"
          >
            {connectors.map((c) => (
              <option key={c.type} value={c.type}>
                {c.display_name}
              </option>
            ))}
          </select>
        </div>

        <Text
          label="Name"
          value={name}
          onChange={invalidate(setName)}
          required
          help="How this platform is labelled throughout the portal."
        />
        <Text
          label={schema?.endpoint_label ?? 'Endpoint URL'}
          value={endpoint}
          onChange={invalidate(setEndpoint)}
          required
          help={schema?.endpoint_help}
        />
        <Text
          label="Datacenter"
          value={datacenter}
          onChange={invalidate(setDatacenter)}
          help="A label for grouping platforms by site. Free text."
        />

        <fieldset className="space-y-2 rounded-lg border border-border p-3">
          <legend className="px-1 text-sm font-medium">Certificate</legend>
          <select
            value={tlsMode}
            onChange={(e) => invalidate(setTLSMode)(e.target.value)}
            className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
          >
            {TLS_MODES.map((mode) => (
              <option key={mode.value} value={mode.value}>
                {mode.label}
              </option>
            ))}
          </select>
          <p className="text-xs text-muted">{TLS_MODES.find((m) => m.value === tlsMode)?.help}</p>
          {tlsMode === 'insecure' && (
            <p className="rounded-md bg-danger/10 p-2 text-xs text-danger">
              Certificates will not be checked. Anyone able to intercept this connection can read
              the credential and the console traffic.
            </p>
          )}
          {tlsMode === 'fingerprint' && (
            <Text
              label="SHA-256 fingerprint"
              value={fingerprint}
              onChange={invalidate(setFingerprint)}
              placeholder="AB:CD:EF:…"
              help={
                editing
                  ? 'Leave empty to keep the stored fingerprint.'
                  : 'Shown in the platform UI, or from: openssl s_client -connect host:port'
              }
            />
          )}
          {tlsMode === 'custom_ca' && (
            <div className="space-y-1">
              <label htmlFor="ca-pem" className="block text-sm">
                CA certificate (PEM)
              </label>
              <textarea
                id="ca-pem"
                value={caPEM}
                onChange={(e) => invalidate(setCAPEM)(e.target.value)}
                rows={5}
                className="w-full rounded-md border border-border bg-surface px-3 py-2 font-mono text-xs"
                placeholder="-----BEGIN CERTIFICATE-----"
              />
            </div>
          )}
        </fieldset>

        {schema?.fields && schema.fields.length > 0 && (
          <fieldset className="space-y-3 rounded-lg border border-border p-3">
            <legend className="px-1 text-sm font-medium">{connector?.display_name} settings</legend>
            <SchemaFields
              fields={schema.fields}
              values={config}
              onChange={(key, value) => {
                setReport(null)
                setConfig((prev) => ({ ...prev, [key]: value }))
              }}
            />
          </fieldset>
        )}

        <fieldset className="space-y-3 rounded-lg border border-border p-3">
          <legend className="px-1 text-sm font-medium">Credentials</legend>
          {(schema?.credentials?.length ?? 0) > 1 && (
            <select
              value={credentialKind}
              onChange={(e) => {
                setCredentialKind(e.target.value)
                setCredentials({})
                setReport(null)
              }}
              className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
            >
              {schema?.credentials?.map((c) => (
                <option key={c.kind} value={c.kind}>
                  {c.label}
                </option>
              ))}
            </select>
          )}
          {credentialForm?.help && <p className="text-xs text-muted">{credentialForm.help}</p>}
          {credentialForm && (
            <SchemaFields
              fields={credentialForm.fields}
              values={credentials}
              secretsStored={editing}
              onChange={(key, value) => {
                setReport(null)
                setCredentials((prev) => ({ ...prev, [key]: value }))
              }}
            />
          )}
        </fieldset>

        {report && <TestResult report={report} />}
        {error && <p className="text-sm text-danger">{error}</p>}
        {missing.length > 0 && (
          <p className="text-xs text-muted">Still needed: {missing.join(', ')}</p>
        )}
      </div>
    </Drawer>
  )
}

/** The point of the test is the detail: "failed" tells an administrator
 *  nothing, whereas "reached it, authenticated, but the token lacks
 *  VM.Console" tells them exactly what to change. */
function TestResult({ report }: { report: TestReport }) {
  const ok = report.reachable && report.authenticated
  return (
    <div
      className={`space-y-2 rounded-lg border p-3 text-sm ${
        ok ? 'border-running/40 bg-running/5' : 'border-danger/40 bg-danger/5'
      }`}
    >
      <Check ok={report.reachable} label="Reachable" />
      <Check ok={report.authenticated} label="Authenticated" />
      {report.version && <p className="text-xs text-muted">Version {report.version}</p>}
      {report.nodes !== undefined && report.nodes > 0 && (
        <p className="text-xs text-muted">{report.nodes} node(s) visible</p>
      )}
      {report.error && <p className="text-xs text-danger">{report.error}</p>}
      {report.missing_permissions && report.missing_permissions.length > 0 && (
        <div className="text-xs">
          <p className="font-medium text-paused">Missing privileges</p>
          <ul className="list-inside list-disc text-muted">
            {report.missing_permissions.map((p) => (
              <li key={p}>
                <span className="font-mono">{p}</span>
              </li>
            ))}
          </ul>
          <p className="mt-1 text-muted">
            The platform can be added without these; the features needing them will fail.
          </p>
        </div>
      )}
      {report.warnings?.map((warning) => (
        <p key={warning} className="text-xs text-paused">
          {warning}
        </p>
      ))}
    </div>
  )
}

function Check({ ok, label }: { ok: boolean; label: string }) {
  return (
    <p className="flex items-center gap-2">
      <span className={ok ? 'text-running' : 'text-danger'}>{ok ? '✓' : '✗'}</span>
      <span>{label}</span>
    </p>
  )
}

function Text({
  label,
  value,
  onChange,
  required,
  help,
  placeholder,
}: {
  label: string
  value: string
  onChange: (value: string) => void
  required?: boolean
  help?: string
  placeholder?: string
}) {
  const id = `f-${label.replace(/\W+/g, '-').toLowerCase()}`
  return (
    <div className="space-y-1">
      <label htmlFor={id} className="block text-sm">
        {label}
        {required && <span className="ml-1 text-danger">*</span>}
      </label>
      <input
        id={id}
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
      />
      {help && <p className="text-xs text-muted">{help}</p>}
    </div>
  )
}
