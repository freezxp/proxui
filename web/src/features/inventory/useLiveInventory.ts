import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { subscribeToEvents, eventVMId, isVMEvent } from '@/lib/events'
import type { Paged, VMListItem, VMState } from '@/api/types'

/**
 * Keeps the inventory current from the live event stream.
 *
 * A state change patches the cached row in place rather than refetching the
 * page: refetching on every event would hammer the API when a platform is
 * churning, and would also yank the table out from under someone mid-read by
 * reordering it. Anything structural (a VM appearing or disappearing) does
 * invalidate, because patching cannot express it.
 */
export function useLiveInventory(): { connected: boolean } {
  const queryClient = useQueryClient()
  const [connected, setConnected] = useState(false)

  useEffect(() => {
    return subscribeToEvents((event) => {
      if (!isVMEvent(event)) return

      if (event.type === 'vm.created' || event.type === 'vm.deleted') {
        void queryClient.invalidateQueries({ queryKey: ['vms'] })
        void queryClient.invalidateQueries({ queryKey: ['dashboard'] })
        return
      }

      if (event.type !== 'vm.state_changed') return
      const vmId = eventVMId(event)
      const nextState = event.payload['to']
      if (!vmId || typeof nextState !== 'string') return

      queryClient.setQueriesData<Paged<VMListItem>>({ queryKey: ['vms'] }, (page) => {
        if (!page) return page
        const index = page.data.findIndex((vm) => vm.id === vmId)
        if (index === -1) return page

        const updated = [...page.data]
        updated[index] = { ...updated[index], state: nextState as VMState }
        return { ...page, data: updated }
      })

      // The dashboard counts by state, so its totals are now wrong.
      void queryClient.invalidateQueries({ queryKey: ['dashboard'] })
    }, setConnected)
  }, [queryClient])

  return { connected }
}
