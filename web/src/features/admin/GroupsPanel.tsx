import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { Grant, UserGroup, VMGroup, VMListItem } from '@/api/types'
import { Drawer } from '@/components/Drawer'

export function GroupsPanel() {
  const queryClient = useQueryClient()
  const [members, setMembers] = useState<VMGroup | null>(null)

  const userGroups = useQuery({
    queryKey: ['user-groups'],
    queryFn: () => api.get<{ data: UserGroup[] }>('/user-groups'),
  })
  const vmGroups = useQuery({
    queryKey: ['vm-groups'],
    queryFn: () => api.get<{ data: VMGroup[] }>('/vm-groups'),
  })
  const grants = useQuery({
    queryKey: ['grants'],
    queryFn: () => api.get<{ data: Grant[] }>('/grants'),
  })

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ['user-groups'] })
    void queryClient.invalidateQueries({ queryKey: ['vm-groups'] })
    void queryClient.invalidateQueries({ queryKey: ['grants'] })
  }

  return (
    <div className="space-y-6">
      <div className="grid gap-4 lg:grid-cols-2">
        <GroupList
          title="User groups"
          description="Who is grouped together."
          groups={userGroups.data?.data ?? []}
          path="/user-groups"
          onChanged={invalidate}
        />
        <GroupList
          title="VM groups"
          description="What can be granted, as a set."
          groups={vmGroups.data?.data ?? []}
          path="/vm-groups"
          onChanged={invalidate}
          onEditMembers={(group) => setMembers(group as VMGroup)}
        />
      </div>

      <GrantsMatrix
        userGroups={userGroups.data?.data ?? []}
        vmGroups={vmGroups.data?.data ?? []}
        grants={grants.data?.data ?? []}
        onChanged={invalidate}
      />

      {members && (
        <MemberPicker
          group={members}
          onClose={() => setMembers(null)}
          onSaved={() => {
            setMembers(null)
            invalidate()
          }}
        />
      )}
    </div>
  )
}

function GroupList({
  title,
  description,
  groups,
  path,
  onChanged,
  onEditMembers,
}: {
  title: string
  description: string
  groups: UserGroup[]
  path: string
  onChanged: () => void
  onEditMembers?: (group: UserGroup) => void
}) {
  const [name, setName] = useState('')
  const [error, setError] = useState('')

  const create = useMutation({
    mutationFn: () => api.post(path, { name: name.trim() }),
    onSuccess: () => {
      setName('')
      setError('')
      onChanged()
    },
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not create the group.'),
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`${path}/${id}`),
    onSuccess: onChanged,
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not delete the group.'),
  })

  return (
    <section className="space-y-3 rounded-lg border border-border p-4">
      <div>
        <h2 className="font-medium">{title}</h2>
        <p className="text-xs text-muted">{description}</p>
      </div>

      <ul className="space-y-1">
        {groups.length === 0 && <li className="text-sm text-muted">None yet.</li>}
        {groups.map((group) => (
          <li
            key={group.id}
            className="flex items-center justify-between rounded-md border border-border px-3 py-2"
          >
            <div>
              <div className="text-sm">{group.name}</div>
              <div className="text-xs text-muted">
                {group.member_count} member{group.member_count === 1 ? '' : 's'}
                {group.member_count === 0 &&
                  onEditMembers &&
                  ' — grants of this group reach nothing'}
              </div>
            </div>
            <div className="space-x-3">
              {onEditMembers && (
                <button
                  onClick={() => onEditMembers(group)}
                  className="text-xs text-accent hover:underline"
                >
                  Members
                </button>
              )}
              <button
                onClick={() => remove.mutate(group.id)}
                className="text-xs text-danger hover:underline"
              >
                Delete
              </button>
            </div>
          </li>
        ))}
      </ul>

      <div className="flex gap-2">
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && name.trim() && create.mutate()}
          placeholder="New group name"
          className="flex-1 rounded-md border border-border bg-surface px-3 py-2 text-sm"
        />
        <button
          onClick={() => create.mutate()}
          disabled={!name.trim() || create.isPending}
          className="rounded-md border border-border px-3 py-2 text-sm disabled:opacity-40"
        >
          Add
        </button>
      </div>
      {error && <p className="text-xs text-danger">{error}</p>}
    </section>
  )
}

/** The grants matrix is the one screen that answers "who can see what" at a
 *  glance, which is why it is a grid rather than a list of pairs. */
function GrantsMatrix({
  userGroups,
  vmGroups,
  grants,
  onChanged,
}: {
  userGroups: UserGroup[]
  vmGroups: VMGroup[]
  grants: Grant[]
  onChanged: () => void
}) {
  const [error, setError] = useState('')
  const byPair = useMemo(() => {
    const map = new Map<string, Grant>()
    for (const grant of grants) map.set(`${grant.user_group_id}:${grant.vm_group_id}`, grant)
    return map
  }, [grants])

  const toggle = useMutation({
    mutationFn: ({ userGroupID, vmGroupID }: { userGroupID: string; vmGroupID: string }) => {
      const existing = byPair.get(`${userGroupID}:${vmGroupID}`)
      return existing
        ? api.del(`/grants/${existing.id}`)
        : api.post('/grants', { user_group_id: userGroupID, vm_group_id: vmGroupID })
    },
    onSuccess: () => {
      setError('')
      onChanged()
    },
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not change the grant.'),
  })

  if (userGroups.length === 0 || vmGroups.length === 0) {
    return (
      <section className="rounded-lg border border-dashed border-border p-6 text-center">
        <p className="text-sm text-muted">
          Create at least one user group and one VM group to grant access between them.
        </p>
      </section>
    )
  }

  return (
    <section className="space-y-3 rounded-lg border border-border p-4">
      <div>
        <h2 className="font-medium">Grants</h2>
        <p className="text-xs text-muted">
          A tick means every member of that user group can see every VM in that group. Roles still
          decide what they may do.
        </p>
      </div>

      <div className="overflow-x-auto">
        <table className="tabular-nums text-sm">
          <thead>
            <tr>
              <th className="px-3 py-2 text-left text-xs uppercase tracking-wide text-muted">
                User group
              </th>
              {vmGroups.map((vmGroup) => (
                <th key={vmGroup.id} className="px-3 py-2 text-center text-xs font-medium">
                  {vmGroup.name}
                  <span className="block font-normal text-muted">{vmGroup.member_count} VMs</span>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {userGroups.map((userGroup) => (
              <tr key={userGroup.id} className="border-t border-border">
                <td className="px-3 py-2">
                  {userGroup.name}
                  <span className="block text-xs text-muted">
                    {userGroup.member_count} user{userGroup.member_count === 1 ? '' : 's'}
                  </span>
                </td>
                {vmGroups.map((vmGroup) => {
                  const granted = byPair.has(`${userGroup.id}:${vmGroup.id}`)
                  return (
                    <td key={vmGroup.id} className="px-3 py-2 text-center">
                      <input
                        type="checkbox"
                        checked={granted}
                        disabled={toggle.isPending}
                        aria-label={`${userGroup.name} may see ${vmGroup.name}`}
                        onChange={() =>
                          toggle.mutate({ userGroupID: userGroup.id, vmGroupID: vmGroup.id })
                        }
                      />
                    </td>
                  )
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {error && <p className="text-xs text-danger">{error}</p>}
    </section>
  )
}

function MemberPicker({
  group,
  onClose,
  onSaved,
}: {
  group: VMGroup
  onClose: () => void
  onSaved: () => void
}) {
  const [search, setSearch] = useState('')
  const [selected, setSelected] = useState<Set<string> | null>(null)
  const [error, setError] = useState('')

  const vms = useQuery({
    queryKey: ['vms-for-groups'],
    queryFn: () => api.get<{ data: VMListItem[] }>('/vms?per_page=500'),
  })
  const current = useQuery({
    queryKey: ['vm-group-members', group.id],
    queryFn: () => api.get<{ data: string[] }>(`/vm-groups/${group.id}/members`),
  })

  // Selection starts from what is stored, once, and is the user's from then on.
  const chosen = selected ?? new Set(current.data?.data ?? [])

  const save = useMutation({
    mutationFn: () => api.put(`/vm-groups/${group.id}/members`, { vm_ids: [...chosen] }),
    onSuccess: onSaved,
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not save the members.'),
  })

  const filtered = (vms.data?.data ?? []).filter((vm) =>
    vm.name.toLowerCase().includes(search.toLowerCase()),
  )

  function toggle(id: string) {
    const next = new Set(chosen)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    setSelected(next)
  }

  return (
    <Drawer
      title={`Members of ${group.name}`}
      onClose={onClose}
      footer={
        <div className="flex items-center justify-between">
          <span className="text-xs text-muted">{chosen.size} selected</span>
          <div className="flex gap-2">
            <button onClick={onClose} className="rounded-md border border-border px-3 py-2 text-sm">
              Cancel
            </button>
            <button
              onClick={() => save.mutate()}
              disabled={save.isPending || current.isLoading}
              className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-40"
            >
              Save members
            </button>
          </div>
        </div>
      }
    >
      <div className="space-y-3">
        <p className="text-xs text-muted">Everyone granted this group sees every VM ticked here.</p>
        <input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search VMs"
          className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
        />

        {vms.isLoading || current.isLoading ? (
          <p className="text-sm text-muted">Loading…</p>
        ) : (
          <ul className="space-y-0.5">
            {filtered.map((vm) => (
              <li key={vm.id}>
                <label className="flex items-center gap-2 rounded-md px-2 py-1 hover:bg-surface-inset">
                  <input
                    type="checkbox"
                    checked={chosen.has(vm.id)}
                    onChange={() => toggle(vm.id)}
                  />
                  <span className="text-sm">{vm.name}</span>
                  <span className="text-xs text-muted">
                    {vm.platform_name}
                    {vm.host_name && ` · ${vm.host_name}`}
                  </span>
                </label>
              </li>
            ))}
            {filtered.length === 0 && <li className="text-sm text-muted">Nothing matches.</li>}
          </ul>
        )}
        {error && <p className="text-sm text-danger">{error}</p>}
      </div>
    </Drawer>
  )
}
