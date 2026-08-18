import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { Setting } from '@/api/types'
import { PortalKeyPanel } from '@/features/shell/PortalKeyPanel'

/** Logos are stored as a data URI in a setting, not uploaded: the file is read
 *  and encoded in the browser, so the portal still accepts no file uploads and
 *  needs no storage for them. 128 KB is generous for a mark and small enough
 *  that a settings row stays a settings row. */
const MAX_LOGO_BYTES = 128 * 1024

export function SettingsPage() {
  const queryClient = useQueryClient()
  const [drafts, setDrafts] = useState<Record<string, number | string>>({})
  const [error, setError] = useState('')

  const settings = useQuery({
    queryKey: ['settings'],
    queryFn: () => api.get<{ data: Setting[] }>('/settings'),
  })

  const save = useMutation({
    mutationFn: ({ item, value }: { item: Setting; value: number | string | null }) => {
      if (value === null) return api.put(`/settings/${item.key}`, {})
      return api.put(
        `/settings/${item.key}`,
        typeof value === 'number' ? { value } : { text: value },
      )
    },
    onSuccess: (_, { item }) => {
      setError('')
      setDrafts((prev) => {
        const next = { ...prev }
        delete next[item.key]
        return next
      })
      void queryClient.invalidateQueries({ queryKey: ['settings'] })
      // The header and the sign-in page read branding from their own query.
      void queryClient.invalidateQueries({ queryKey: ['branding'] })
    },
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not save the setting.'),
  })

  if (settings.isLoading) return <p className="text-sm text-muted">Loading…</p>

  const groups = new Map<string, Setting[]>()
  for (const item of settings.data?.data ?? []) {
    groups.set(item.group, [...(groups.get(item.group) ?? []), item])
  }

  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-xl font-semibold">Settings</h1>
        <p className="text-sm text-muted">
          Changes apply immediately; nothing here needs a restart.
        </p>
      </div>

      {error && <p className="text-sm text-danger">{error}</p>}

      <PortalKeyPanel />

      {[...groups.entries()].map(([group, items]) => (
        <section key={group} className="space-y-4 rounded-lg border border-border p-4">
          <h2 className="text-sm font-medium">{group}</h2>
          {items.map((item) => (
            <SettingRow
              key={item.key}
              item={item}
              draft={drafts[item.key]}
              onDraft={(value) => setDrafts((prev) => ({ ...prev, [item.key]: value }))}
              onSave={(value) => save.mutate({ item, value })}
              onError={setError}
            />
          ))}
        </section>
      ))}
    </div>
  )
}

function SettingRow({
  item,
  draft,
  onDraft,
  onSave,
  onError,
}: {
  item: Setting
  draft: number | string | undefined
  onDraft: (value: number | string) => void
  onSave: (value: number | string | null) => void
  onError: (message: string) => void
}) {
  if (item.kind === 'image') {
    return <ImageSetting item={item} onSave={onSave} onError={onError} />
  }
  if (item.kind === 'secret') {
    return <SecretSetting item={item} onSave={onSave} />
  }

  const stored = item.kind === 'text' ? (item.text ?? '') : (item.value ?? 0)
  const current = draft ?? stored
  const dirty = current !== stored

  return (
    <div className="space-y-1">
      <div className="flex flex-wrap items-center gap-2">
        <label htmlFor={item.key} className="min-w-56 text-sm">
          {item.label}
          {item.modified && (
            <span className="ml-2 rounded-full bg-accent/10 px-2 py-0.5 text-xs text-accent">
              modified
            </span>
          )}
        </label>

        {item.kind === 'select' ? (
          <select
            id={item.key}
            value={String(current)}
            onChange={(e) => onDraft(e.target.value)}
            className="w-96 rounded-md border border-border bg-surface px-3 py-1.5 text-sm"
          >
            {item.options?.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        ) : item.kind === 'text' ? (
          <input
            id={item.key}
            value={String(current)}
            maxLength={item.max_length}
            // An empty field that falls back to something should say what,
            // rather than looking like a field nobody filled in.
            placeholder={emptyMeans(item)}
            onChange={(e) => onDraft(e.target.value)}
            className="w-72 rounded-md border border-border bg-surface px-3 py-1.5 text-sm"
          />
        ) : (
          <>
            <input
              id={item.key}
              type="number"
              value={Number(current)}
              min={item.min}
              max={item.max}
              onChange={(e) => onDraft(Number(e.target.value))}
              className="w-32 rounded-md border border-border bg-surface px-3 py-1.5 text-sm"
            />
            <span className="text-xs text-muted">{unitLabel(item)}</span>
          </>
        )}

        {dirty && (
          <button
            onClick={() => onSave(current)}
            className="rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-white"
          >
            Save
          </button>
        )}
        {item.modified && !dirty && (
          <button onClick={() => onSave(null)} className="text-xs text-muted hover:underline">
            Reset{item.kind === 'text' ? '' : ` to ${formatValue(item, item.default ?? 0)}`}
          </button>
        )}
      </div>
      <p className="text-xs text-muted">
        {item.help}
        {item.kind !== 'text' &&
          ` Allowed: ${formatValue(item, item.min ?? 0)} to ${formatValue(item, item.max ?? 0)}.`}
      </p>
    </div>
  )
}

/** A secret is write-only: the value is never sent back, so the field offers
 *  to replace it rather than pretending to show it. Same treatment a platform
 *  credential gets. */
function SecretSetting({
  item,
  onSave,
}: {
  item: Setting
  onSave: (value: string | null) => void
}) {
  const [draft, setDraft] = useState('')
  const [editing, setEditing] = useState(false)

  return (
    <div className="space-y-1">
      <div className="flex flex-wrap items-center gap-2">
        <label htmlFor={item.key} className="min-w-56 text-sm">
          {item.label}
          {item.has_value && (
            <span className="ml-2 rounded-full bg-state-running/15 px-2 py-0.5 text-xs text-state-running">
              set
            </span>
          )}
        </label>

        {editing || !item.has_value ? (
          <>
            <input
              id={item.key}
              type="password"
              value={draft}
              autoComplete="new-password"
              placeholder={item.has_value ? 'Enter a new secret' : ''}
              onChange={(e) => setDraft(e.target.value)}
              className="w-96 rounded-md border border-border bg-surface px-3 py-1.5 text-sm"
            />
            <button
              onClick={() => {
                onSave(draft)
                setDraft('')
                setEditing(false)
              }}
              disabled={!draft}
              className="rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-white disabled:opacity-40"
            >
              Save
            </button>
            {item.has_value && (
              <button
                onClick={() => {
                  setDraft('')
                  setEditing(false)
                }}
                className="text-xs text-muted hover:underline"
              >
                Cancel
              </button>
            )}
          </>
        ) : (
          <>
            <span className="font-mono text-sm text-muted">••••••••••••</span>
            <button
              onClick={() => setEditing(true)}
              className="text-xs text-accent hover:underline"
            >
              Replace
            </button>
            <button onClick={() => onSave(null)} className="text-xs text-danger hover:underline">
              Remove
            </button>
          </>
        )}
      </div>
      <p className="text-xs text-muted">{item.help}</p>
    </div>
  )
}

function ImageSetting({
  item,
  onSave,
  onError,
}: {
  item: Setting
  onSave: (value: string | null) => void
  onError: (message: string) => void
}) {
  const input = useRef<HTMLInputElement>(null)
  const current = item.text ?? ''

  function choose(file: File | undefined) {
    if (!file) return
    if (file.size > MAX_LOGO_BYTES) {
      onError(`That image is ${Math.round(file.size / 1024)} KB; the limit is 128 KB.`)
      return
    }
    const reader = new FileReader()
    reader.onload = () => onSave(String(reader.result))
    reader.onerror = () => onError('That file could not be read.')
    // Read as a data URI: the value stored is the image itself, so there is
    // nothing to serve, back up separately, or lose.
    reader.readAsDataURL(file)
  }

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-3">
        <span className="min-w-56 text-sm">
          {item.label}
          {item.modified && (
            <span className="ml-2 rounded-full bg-accent/10 px-2 py-0.5 text-xs text-accent">
              modified
            </span>
          )}
        </span>

        <div className="flex h-12 w-12 items-center justify-center rounded-md border border-border bg-surface">
          {current ? (
            <img src={current} alt="Current logo" className="max-h-10 max-w-10" />
          ) : (
            <span className="text-[10px] text-muted">none</span>
          )}
        </div>

        <input
          ref={input}
          type="file"
          accept="image/png,image/jpeg,image/svg+xml,image/webp"
          className="hidden"
          onChange={(e) => choose(e.target.files?.[0])}
        />
        <button
          onClick={() => input.current?.click()}
          className="rounded-md border border-border px-3 py-1.5 text-sm"
        >
          Choose image…
        </button>
        {current && (
          <button onClick={() => onSave(null)} className="text-xs text-danger hover:underline">
            Remove
          </button>
        )}
      </div>
      <p className="text-xs text-muted">
        {item.help} PNG, JPEG, WebP or SVG, up to 128 KB. The image is stored in the portal, so it
        keeps working without reaching any other site.
      </p>
    </div>
  )
}

// What an empty text field resolves to, shown as its placeholder.
function emptyMeans(item: Setting): string {
  if (item.key === 'branding.portal_name') return window.location.hostname
  // The redirect URL has exactly one correct value for this deployment, and
  // typing it by hand is how it ends up mismatched.
  if (item.key === 'auth.google_redirect_url') {
    return `${window.location.origin}/api/v1/auth/google/callback`
  }
  return ''
}

function unitLabel(item: Setting): string {
  if (item.kind === 'days') return 'days'
  if (item.kind === 'count') return 'attempts'
  return 'seconds'
}

// Ranges are written in whatever unit reads naturally: "2592000 seconds" tells
// nobody it is a month.
function formatValue(item: Setting, value: number): string {
  if (item.kind === 'days') return `${value} days`
  if (item.kind === 'count') return String(value)
  if (value >= 86400) return `${Math.round(value / 86400)} days`
  if (value >= 3600) return `${Math.round(value / 3600)} hours`
  if (value >= 60) return `${Math.round(value / 60)} minutes`
  return `${value} seconds`
}
