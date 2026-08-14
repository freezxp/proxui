import type { SchemaField } from '@/api/types'

export type Values = Record<string, unknown>

/** Renders the fields a connector declared. Nothing here knows what a Proxmox
 *  is: adding a platform means declaring fields on the connector, not editing
 *  this file (docs/09-connector-architecture.md). */
export function SchemaFields({
  fields,
  values,
  onChange,
  // When editing, a stored secret is never sent back to the browser. An empty
  // secret box therefore means "keep what is stored", not "clear it", and it
  // has to say so or an administrator will assume the credential was lost.
  secretsStored,
}: {
  fields: SchemaField[]
  values: Values
  onChange: (key: string, value: unknown) => void
  secretsStored?: boolean
}) {
  return (
    <>
      {fields.map((field) => (
        <Field
          key={field.key}
          field={field}
          values={values}
          onChange={onChange}
          secretsStored={secretsStored}
        />
      ))}
    </>
  )
}

function Field({
  field,
  values,
  onChange,
  secretsStored,
}: {
  field: SchemaField
  values: Values
  onChange: (key: string, value: unknown) => void
  secretsStored?: boolean
}) {
  const value = values[field.key]
  const id = `field-${field.key}`

  if (field.kind === 'bool') {
    return (
      <label className="flex items-start gap-2 py-1" htmlFor={id}>
        <input
          id={id}
          type="checkbox"
          checked={Boolean(value)}
          onChange={(e) => onChange(field.key, e.target.checked)}
          className="mt-0.5"
        />
        <span>
          <span className="text-sm">{field.label}</span>
          {field.help && <span className="block text-xs text-muted">{field.help}</span>}
        </span>
      </label>
    )
  }

  return (
    <div className="space-y-1">
      <label htmlFor={id} className="block text-sm">
        {field.label}
        {field.required && <span className="ml-1 text-danger">*</span>}
      </label>

      {field.kind === 'select' ? (
        <select
          id={id}
          value={String(value ?? '')}
          onChange={(e) => onChange(field.key, e.target.value)}
          className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
        >
          {field.options?.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
      ) : (
        <input
          id={id}
          type={field.kind === 'secret' ? 'password' : field.kind === 'number' ? 'number' : 'text'}
          value={String(value ?? '')}
          placeholder={
            field.kind === 'secret' && secretsStored ? 'unchanged' : (field.placeholder ?? '')
          }
          autoComplete={field.kind === 'secret' ? 'new-password' : 'off'}
          onChange={(e) =>
            onChange(field.key, field.kind === 'number' ? Number(e.target.value) : e.target.value)
          }
          className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
        />
      )}

      {field.help && <p className="text-xs text-muted">{field.help}</p>}
      {field.kind === 'secret' && secretsStored && (
        <p className="text-xs text-muted">Leave empty to keep the stored secret.</p>
      )}
    </div>
  )
}

/** Applies a schema's declared defaults, so a form opens the way the connector
 *  says it should rather than empty. */
export function defaultsFor(fields: SchemaField[] | undefined): Values {
  const out: Values = {}
  for (const field of fields ?? []) {
    if (field.default !== undefined) out[field.key] = field.default
    else if (field.kind === 'bool') out[field.key] = false
    else out[field.key] = ''
  }
  return out
}

/** Reports which required fields are still empty, so Save can say what is
 *  missing instead of failing at the server. */
export function missingRequired(fields: SchemaField[] | undefined, values: Values): string[] {
  return (fields ?? [])
    .filter((f) => f.required && !String(values[f.key] ?? '').trim())
    .map((f) => f.label)
}
