import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { User, UserGroup } from '@/api/types'
import { Drawer } from '@/components/Drawer'
import { relativeTime } from '@/lib/format'
import { GroupsPanel } from './GroupsPanel'

type Tab = 'users' | 'groups'

const ROLES: { value: User['role']; label: string; help: string }[] = [
  {
    value: 'admin',
    label: 'Administrator',
    help: 'Everything, including user and platform management.',
  },
  {
    value: 'operator',
    label: 'Operator',
    help: 'Consoles and power actions, on granted VMs only.',
  },
  { value: 'readonly', label: 'Read only', help: 'Can look at granted VMs; cannot act on them.' },
  { value: 'auditor', label: 'Auditor', help: 'Read access plus the audit log.' },
]

export function UsersPage() {
  const [tab, setTab] = useState<Tab>('users')

  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-xl font-semibold">Users &amp; groups</h1>
        <p className="text-sm text-muted">
          Roles decide what someone may do. Grants decide which machines they may do it to.
        </p>
      </div>

      <nav className="flex gap-1 border-b border-border">
        {(['users', 'groups'] as Tab[]).map((name) => (
          <button
            key={name}
            onClick={() => setTab(name)}
            className={`-mb-px border-b-2 px-4 py-2 text-sm capitalize ${
              tab === name
                ? 'border-accent font-medium text-accent'
                : 'border-transparent text-muted hover:text-content'
            }`}
          >
            {name === 'groups' ? 'Groups & grants' : 'Users'}
          </button>
        ))}
      </nav>

      {tab === 'users' ? <UsersTab /> : <GroupsPanel />}
    </div>
  )
}

function UsersTab() {
  const [editing, setEditing] = useState<User | 'new' | null>(null)
  const [resetting, setResetting] = useState<User | null>(null)
  const queryClient = useQueryClient()

  const users = useQuery({
    queryKey: ['users'],
    queryFn: () => api.get<{ data: User[] }>('/users'),
  })
  const groups = useQuery({
    queryKey: ['user-groups'],
    queryFn: () => api.get<{ data: UserGroup[] }>('/user-groups'),
  })

  const setActive = useMutation({
    mutationFn: ({ user, active }: { user: User; active: boolean }) =>
      api.put<User>(`/users/${user.id}`, { is_active: active }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['users'] }),
  })

  if (users.isLoading) return <p className="text-sm text-muted">Loading…</p>

  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <button
          onClick={() => setEditing('new')}
          className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white"
        >
          Add user
        </button>
      </div>

      <div className="overflow-hidden rounded-lg border border-border">
        <table className="w-full text-sm">
          <thead className="bg-surface-raised text-left text-xs uppercase tracking-wide text-muted">
            <tr>
              <th className="px-4 py-2 font-medium">User</th>
              <th className="px-4 py-2 font-medium">Role</th>
              <th className="px-4 py-2 font-medium">Last login</th>
              <th className="px-4 py-2 font-medium">Status</th>
              <th className="px-4 py-2" />
            </tr>
          </thead>
          <tbody>
            {users.data?.data.map((user) => (
              <tr key={user.id} className="border-t border-border">
                <td className="px-4 py-2">
                  <div className="font-medium">{user.display_name || user.username}</div>
                  <div className="text-xs text-muted">
                    {user.username} · {user.email}
                  </div>
                </td>
                <td className="px-4 py-2">
                  {ROLES.find((r) => r.value === user.role)?.label ?? user.role}
                </td>
                <td className="px-4 py-2 text-muted">
                  {user.last_login_at ? relativeTime(user.last_login_at) : 'never'}
                </td>
                <td className="px-4 py-2">
                  <div className="flex flex-wrap gap-1">
                    <span
                      className={`rounded-full px-2 py-0.5 text-xs ${
                        user.is_active
                          ? 'bg-state-running/15 text-state-running'
                          : 'bg-state-stopped/15 text-state-stopped'
                      }`}
                    >
                      {user.is_active ? 'active' : 'disabled'}
                    </span>
                    {user.must_change_password && (
                      <span
                        className="rounded-full bg-state-paused/15 px-2 py-0.5 text-xs text-state-paused"
                        title="This account must set a new password at next sign-in."
                      >
                        password pending
                      </span>
                    )}
                  </div>
                </td>
                <td className="space-x-3 px-4 py-2 text-right">
                  <button
                    onClick={() => setEditing(user)}
                    className="text-xs text-accent hover:underline"
                  >
                    Edit
                  </button>
                  <button
                    onClick={() => setResetting(user)}
                    className="text-xs text-accent hover:underline"
                  >
                    Reset password
                  </button>
                  <button
                    onClick={() => setActive.mutate({ user, active: !user.is_active })}
                    className="text-xs text-muted hover:underline"
                  >
                    {user.is_active ? 'Disable' : 'Enable'}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {resetting && (
        <ResetPasswordDrawer
          user={resetting}
          onClose={() => setResetting(null)}
          onDone={() => {
            setResetting(null)
            void queryClient.invalidateQueries({ queryKey: ['users'] })
          }}
        />
      )}

      {editing && (
        <UserForm
          user={editing === 'new' ? null : editing}
          groups={groups.data?.data ?? []}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null)
            void queryClient.invalidateQueries({ queryKey: ['users'] })
          }}
        />
      )}
    </div>
  )
}

/** Generates a temporary password rather than asking an administrator to
 *  invent one. A human-chosen "temporary" password is usually weak and often
 *  reused, and this one only has to survive until the user changes it. */
function generatePassword(): string {
  const alphabet = 'abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789'
  const bytes = new Uint32Array(20)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (n) => alphabet[n % alphabet.length]).join('')
}

function ResetPasswordDrawer({
  user,
  onClose,
  onDone,
}: {
  user: User
  onClose: () => void
  onDone: () => void
}) {
  const [password, setPassword] = useState(generatePassword)
  const [issued, setIssued] = useState(false)
  const [error, setError] = useState('')

  const reset = useMutation({
    mutationFn: () => api.post(`/users/${user.id}/password`, { temp_password: password }),
    onSuccess: () => setIssued(true),
    onError: (err) =>
      setError(err instanceof Error ? err.message : 'Could not reset the password.'),
  })

  if (issued) {
    return (
      <Drawer title="Password reset" onClose={onDone}>
        <div className="space-y-3">
          <p className="text-sm">
            <span className="font-medium">{user.username}</span> can sign in with this password and
            will be asked to choose their own.
          </p>
          <div className="rounded-md border border-border bg-surface-raised p-3">
            <div className="text-xs uppercase tracking-wide text-muted">Temporary password</div>
            <div className="font-mono text-sm">{password}</div>
          </div>
          <p className="text-xs text-muted">
            This is the only time it is shown. Their existing sessions have been signed out.
          </p>
          <button
            onClick={onDone}
            className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white"
          >
            Done
          </button>
        </div>
      </Drawer>
    )
  }

  return (
    <Drawer
      title={`Reset password for ${user.username}`}
      onClose={onClose}
      footer={
        <div className="flex justify-end gap-2">
          <button onClick={onClose} className="rounded-md border border-border px-3 py-2 text-sm">
            Cancel
          </button>
          <button
            onClick={() => reset.mutate()}
            disabled={password.length < 12 || reset.isPending}
            className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-40"
          >
            {reset.isPending ? 'Resetting…' : 'Reset password'}
          </button>
        </div>
      }
    >
      <div className="space-y-4">
        <p className="text-sm text-muted">
          This signs {user.username} out everywhere and requires them to choose a new password at
          their next sign-in. Use it when someone is locked out or a password may have been seen.
        </p>

        <div className="space-y-1">
          <label htmlFor="temp-password" className="block text-sm">
            Temporary password
          </label>
          <div className="flex gap-2">
            <input
              id="temp-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className="flex-1 rounded-md border border-border bg-surface px-3 py-2 font-mono text-sm"
            />
            <button
              onClick={() => setPassword(generatePassword())}
              className="rounded-md border border-border px-3 py-2 text-sm"
            >
              Regenerate
            </button>
          </div>
          <p className="text-xs text-muted">
            Generated for you. Shown once after the reset, so have somewhere to put it.
          </p>
        </div>

        {error && <p className="text-sm text-danger">{error}</p>}
      </div>
    </Drawer>
  )
}

function UserForm({
  user,
  groups,
  onClose,
  onSaved,
}: {
  user: User | null
  groups: UserGroup[]
  onClose: () => void
  onSaved: () => void
}) {
  const editing = user !== null
  const [username, setUsername] = useState(user?.username ?? '')
  const [email, setEmail] = useState(user?.email ?? '')
  const [displayName, setDisplayName] = useState(user?.display_name ?? '')
  const [role, setRole] = useState<User['role']>(user?.role ?? 'readonly')
  const [password, setPassword] = useState('')
  const [selected, setSelected] = useState<string[]>([])
  const [error, setError] = useState('')
  // Shown once, after creation. There is no way to retrieve it later, so the
  // form says so rather than letting an administrator close the panel and
  // discover the account is unusable.
  const [issued, setIssued] = useState<{ username: string; password: string } | null>(null)

  const create = useMutation({
    mutationFn: () =>
      api.post<User>('/users', {
        username,
        email,
        display_name: displayName,
        role,
        temp_password: password,
        group_ids: selected,
      }),
    onSuccess: () => setIssued({ username, password }),
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not create the user.'),
  })

  const update = useMutation({
    mutationFn: async () => {
      await api.put<User>(`/users/${user!.id}`, {
        display_name: displayName,
        email,
        role,
      })
      await api.put(`/users/${user!.id}/groups`, { group_ids: selected })
    },
    onSuccess: onSaved,
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not save the user.'),
  })

  if (issued) {
    return (
      <Drawer title="User created" onClose={onSaved}>
        <div className="space-y-3">
          <p className="text-sm">
            <span className="font-medium">{issued.username}</span> can now sign in and will be asked
            to choose a new password.
          </p>
          <div className="rounded-md border border-border bg-surface-raised p-3">
            <div className="text-xs uppercase tracking-wide text-muted">Temporary password</div>
            <div className="font-mono text-sm">{issued.password}</div>
          </div>
          <p className="text-xs text-muted">
            This is the only time it is shown. If it is lost, set a new one from the user's edit
            panel.
          </p>
          <button
            onClick={onSaved}
            className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white"
          >
            Done
          </button>
        </div>
      </Drawer>
    )
  }

  const missing = [
    ...(!username.trim() ? ['Username'] : []),
    ...(!email.trim() ? ['Email'] : []),
    ...(!editing && password.length < 12 ? ['A temporary password of at least 12 characters'] : []),
  ]

  return (
    <Drawer
      title={editing ? `Edit ${user.username}` : 'Add user'}
      onClose={onClose}
      footer={
        <div className="flex justify-end gap-2">
          <button onClick={onClose} className="rounded-md border border-border px-3 py-2 text-sm">
            Cancel
          </button>
          <button
            onClick={() => (editing ? update.mutate() : create.mutate())}
            disabled={missing.length > 0 || create.isPending || update.isPending}
            title={missing.length > 0 ? `Still needed: ${missing.join(', ')}` : undefined}
            className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-40"
          >
            {editing ? 'Save changes' : 'Create user'}
          </button>
        </div>
      }
    >
      <div className="space-y-4">
        <Text
          label="Username"
          value={username}
          onChange={setUsername}
          disabled={editing}
          required
        />
        <Text label="Email" value={email} onChange={setEmail} required />
        <Text label="Display name" value={displayName} onChange={setDisplayName} />

        <div className="space-y-1">
          <span className="block text-sm">Role</span>
          {ROLES.map((option) => (
            <label key={option.value} className="flex items-start gap-2 py-0.5">
              <input
                type="radio"
                name="role"
                checked={role === option.value}
                onChange={() => setRole(option.value)}
                className="mt-1"
              />
              <span>
                <span className="text-sm">{option.label}</span>
                <span className="block text-xs text-muted">{option.help}</span>
              </span>
            </label>
          ))}
        </div>

        {!editing && (
          <Text
            label="Temporary password"
            value={password}
            onChange={setPassword}
            type="password"
            required
            help="Shown once after creation. The user must change it at first sign-in."
          />
        )}

        <fieldset className="space-y-2 rounded-lg border border-border p-3">
          <legend className="px-1 text-sm font-medium">Groups</legend>
          {groups.length === 0 ? (
            <p className="text-xs text-muted">
              No user groups exist yet. Without one, this account is granted no VMs at all.
            </p>
          ) : (
            groups.map((group) => (
              <label key={group.id} className="flex items-center gap-2">
                <input
                  type="checkbox"
                  checked={selected.includes(group.id)}
                  onChange={(e) =>
                    setSelected((prev) =>
                      e.target.checked ? [...prev, group.id] : prev.filter((id) => id !== group.id),
                    )
                  }
                />
                <span className="text-sm">{group.name}</span>
              </label>
            ))
          )}
          {editing && (
            <p className="text-xs text-muted">
              Group membership shown here is what will be saved; it replaces the current set.
            </p>
          )}
        </fieldset>

        {error && <p className="text-sm text-danger">{error}</p>}
      </div>
    </Drawer>
  )
}

function Text({
  label,
  value,
  onChange,
  required,
  help,
  type = 'text',
  disabled,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  required?: boolean
  help?: string
  type?: string
  disabled?: boolean
}) {
  const id = `u-${label.replace(/\W+/g, '-').toLowerCase()}`
  return (
    <div className="space-y-1">
      <label htmlFor={id} className="block text-sm">
        {label}
        {required && <span className="ml-1 text-danger">*</span>}
      </label>
      <input
        id={id}
        type={type}
        value={value}
        disabled={disabled}
        autoComplete={type === 'password' ? 'new-password' : 'off'}
        onChange={(e) => onChange(e.target.value)}
        className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm disabled:opacity-60"
      />
      {help && <p className="text-xs text-muted">{help}</p>}
    </div>
  )
}
