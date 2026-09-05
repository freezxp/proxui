import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'

/** A star that pins a VM to the top of the list (INV-16).
 *
 *  The pinning itself is the server's doing — the list is paginated, so
 *  favourites have to be sorted server-side or a starred VM on page 4 would
 *  never reach page 1. All this does is toggle the flag and ask for the list
 *  again.
 *
 *  The toggle is optimistic and reverts on failure, because a star that takes a
 *  round trip to light up feels broken; but a star that lit up and did not save
 *  would be worse, so failure puts it back.
 */
export function FavouriteStar({
  vmID,
  isFavourite,
  invalidate = ['vms'],
}: {
  vmID: string
  isFavourite: boolean
  /** The query key to refresh once the server has agreed. */
  invalidate?: string[]
}) {
  const queryClient = useQueryClient()

  const toggle = useMutation({
    mutationFn: (next: boolean) =>
      next ? api.put(`/vms/${vmID}/favourite`) : api.del(`/vms/${vmID}/favourite`),
    onSettled: () => queryClient.invalidateQueries({ queryKey: invalidate }),
  })

  // While the request is in flight, show where it is going rather than where it
  // has been.
  const shown = toggle.isPending ? !isFavourite : isFavourite

  return (
    <button
      onClick={(e) => {
        // The row is a link to the VM; starring it is not navigation.
        e.preventDefault()
        e.stopPropagation()
        toggle.mutate(!isFavourite)
      }}
      title={shown ? 'Remove from favourites' : 'Add to favourites — favourites sort to the top'}
      aria-label={shown ? 'Remove from favourites' : 'Add to favourites'}
      aria-pressed={shown}
      className={`rounded p-1 text-base leading-none transition-colors ${
        shown ? 'text-state-paused' : 'text-muted/40 hover:text-muted'
      }`}
    >
      {shown ? '★' : '☆'}
    </button>
  )
}
