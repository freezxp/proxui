import { useCallback, useEffect, useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import { api, ApiError, detailOf, releaseOnUnload } from '@/api/client'
import type {
  PortalKeyInstall,
  SshHostKeyMismatch,
  SshHostKeyPrompt,
  SshSession,
  VMDetail,
  VMPortalKeyState,
} from '@/api/types'
import { canReadClipboard, copyText, readText } from '@/lib/clipboard'
import { useViewportHeight } from '@/lib/viewport'
import { FileBrowser } from './FileBrowser'
import { SshTerminal, type Modifiers, type SnapshotScope, type TerminalHandle } from './SshTerminal'
import { TerminalKeys } from './TerminalKeys'
import { TerminalSelect } from './TerminalSelect'

/**
 * The SSH page: a connect form, then a terminal and a file panel (SSH-01).
 *
 * The credential is typed here and posted once. The portal opens the
 * connection with it and never keeps it (SSH-03), which is why a reconnect
 * asks again and why closing the tab does not silently leave a way back in.
 */

type Phase = 'form' | 'connecting' | 'connected' | 'closed' | 'error'

type Auth = 'password' | 'key' | 'portal'

const FONT_SIZES = [11, 12, 13, 14, 16, 18, 20]

export function SshPage() {
  const { vmId = '' } = useParams()
  const terminal = useRef<TerminalHandle>(null)

  const [vm, setVM] = useState<VMDetail | null>(null)
  const [phase, setPhase] = useState<Phase>('form')
  const [message, setMessage] = useState('')
  const [session, setSession] = useState<SshSession | null>(null)

  const [username, setUsername] = useState('root')
  const [auth, setAuth] = useState<Auth>('password')
  const [password, setPassword] = useState('')
  const [privateKey, setPrivateKey] = useState('')
  const [passphrase, setPassphrase] = useState('')
  const [host, setHost] = useState('')
  const [port, setPort] = useState('22')

  const [keyState, setKeyState] = useState<VMPortalKeyState | null>(null)
  const [keyBusy, setKeyBusy] = useState(false)

  const [prompt, setPrompt] = useState<SshHostKeyPrompt | null>(null)
  const [mismatch, setMismatch] = useState<SshHostKeyMismatch | null>(null)

  const [filesOpen, setFilesOpen] = useState(false)
  // The key bar defaults on where the keyboard has no arrows and no Ctrl —
  // a touchscreen — and is a toggle everywhere, because a laptop with a
  // touchscreen is still a laptop and a desktop operator may still want ^C
  // within reach (SSH-08).
  const [keysOpen, setKeysOpen] = useState(
    () => window.matchMedia?.('(pointer: coarse)').matches ?? false,
  )
  const [modifiers, setModifiers] = useState<Modifiers>({ ctrl: false, alt: false })
  const [fontSize, setFontSize] = useState(13)
  const [notice, setNotice] = useState('')
  const [pastePanel, setPastePanel] = useState(false)
  const [pasteText, setPasteText] = useState('')
  const [copyPanel, setCopyPanel] = useState('')
  // The select panel holds a snapshot rather than reading the terminal as it
  // renders: see TerminalSelect for why a live one would be unusable.
  const [selectPanel, setSelectPanel] = useState<string | null>(null)
  const [selectScope, setSelectScope] = useState<SnapshotScope>('screen')
  const [mouseGrabbed, setMouseGrabbed] = useState(false)
  const noticeTimer = useRef<number | undefined>(undefined)

  const flash = useCallback((text: string) => {
    setNotice(text)
    window.clearTimeout(noticeTimer.current)
    noticeTimer.current = window.setTimeout(() => setNotice(''), 2500)
  }, [])
  useEffect(() => () => window.clearTimeout(noticeTimer.current), [])

  useEffect(() => {
    if (vmId === '') return
    api
      .get<VMDetail>(`/vms/${vmId}`)
      .then((detail) => {
        setVM(detail)
        document.title = `${detail.name} — SSH`
      })
      .catch(() => setVM(null))
  }, [vmId])

  // Which accounts on this guest already carry the portal's key. Knowing it
  // before the form renders is what lets key auth be the default where it will
  // actually work, instead of an option that fails for most people who pick it.
  const loadKeyState = useCallback(async () => {
    if (vmId === '') return
    try {
      setKeyState(await api.get<VMPortalKeyState>(`/vms/${vmId}/ssh-key`))
    } catch {
      // A portal with no key, or a role that may not ask, simply gets the
      // password form it had before.
      setKeyState(null)
    }
  }, [vmId])

  useEffect(() => {
    void loadKeyState()
  }, [loadKeyState])

  const installedFor = useCallback(
    (user: string): PortalKeyInstall | undefined =>
      keyState?.data.find((entry) => entry.ssh_user === user && !entry.stale),
    [keyState],
  )
  const keyReady = keyState?.key_exists === true && installedFor(username) !== undefined

  // Preselect the key where it is installed for the account being typed. Only
  // as an initial nudge: once someone has picked a method by hand, changing
  // the username must not silently switch it back under them.
  const autoChosen = useRef(false)
  useEffect(() => {
    if (autoChosen.current || !keyReady) return
    autoChosen.current = true
    setAuth('portal')
  }, [keyReady])

  const connect = useCallback(
    async (acceptFingerprint?: string) => {
      setPhase('connecting')
      setMessage('')
      setMismatch(null)
      try {
        const opened = await api.post<SshSession>(`/vms/${vmId}/ssh`, {
          username,
          password: auth === 'password' ? password : '',
          private_key: auth === 'key' ? privateKey : '',
          passphrase: auth === 'key' ? passphrase : '',
          use_portal_key: auth === 'portal',
          host,
          port: Number(port) || 22,
          accept_host_key: acceptFingerprint ?? '',
        })
        setPrompt(null)
        setSession(opened)
        // The credential has done its work. Dropping it here does not make the
        // portal safer — the server already has what it needs — but it keeps
        // it out of a React state tree that survives until the tab closes.
        setPassword('')
        setPrivateKey('')
        setPassphrase('')
        setPhase('connected')
      } catch (err) {
        if (!(err instanceof ApiError)) {
          setPhase('error')
          setMessage('The connection could not be opened.')
          return
        }
        if (err.code === 'ssh.host_key_unknown') {
          const body = (err.body as { body?: SshHostKeyPrompt })?.body
          if (body) {
            setPrompt(body)
            setPhase('form')
            return
          }
        }
        if (err.code === 'ssh.host_key_mismatch') {
          const body = (err.body as { body?: SshHostKeyMismatch })?.body
          if (body) {
            setMismatch(body)
            setPhase('form')
            return
          }
        }
        setPhase('form')
        setMessage(err.detail)
      }
    },
    [vmId, username, auth, password, privateKey, passphrase, host, port],
  )

  // Installing runs over the session that is already open, with the guest
  // account's own permissions: the portal is not reaching into a machine
  // nobody let it into (SSH-12).
  const keyAction = useCallback(
    async (install: boolean) => {
      if (!session) return
      const path = `/ssh-sessions/${session.session_id}/portal-key`
      setKeyBusy(true)
      try {
        await (install ? api.post(path, {}) : api.del(path))
        await loadKeyState()
        flash(
          install
            ? 'Portal key installed. Next time, connect without a password.'
            : 'Portal key removed from this account.',
        )
      } catch (err) {
        flash(
          detailOf(
            err,
            install ? 'The key could not be installed.' : 'The key could not be removed.',
          ),
        )
      } finally {
        setKeyBusy(false)
      }
    },
    [session, loadKeyState, flash],
  )

  const disconnect = useCallback(async () => {
    if (!session) return
    try {
      await api.del(`/ssh-sessions/${session.session_id}`)
    } catch {
      /* already gone, which is the state we wanted */
    }
    setSession(null)
    setFilesOpen(false)
    setPhase('form')
    setMessage('The session was closed.')
  }, [session])

  // Give the session back when this page goes away.
  //
  // The session id is held in this component and nowhere else, so a tab that
  // closes without telling the server strands an authenticated shell that
  // nobody — not even the operator who opened it — can ever reach again. It
  // would sit there holding one of their eight slots until the sweep took it,
  // which is how someone who opened a few terminals over an afternoon ends up
  // unable to open another (SSH-07).
  //
  // Both ways out are covered: navigating elsewhere in the portal unmounts,
  // and closing the tab fires pagehide. Neither is guaranteed —
  // pagehide does not run for a killed tab, and an unload request can be
  // dropped — so this is an optimisation of the server's sweep rather than a
  // replacement for it.
  const openSession = useRef<string | null>(null)
  openSession.current = session?.session_id ?? null

  useEffect(() => {
    const release = () => {
      const id = openSession.current
      if (id === null) return
      // Cleared first, so the unmount that follows a pagehide does not send
      // the same release twice.
      openSession.current = null
      releaseOnUnload(`/ssh-sessions/${id}`)
    }
    window.addEventListener('pagehide', release)
    return () => {
      window.removeEventListener('pagehide', release)
      release()
    }
  }, [])

  // Copy: try the clipboard, and fall back to a panel the operator can copy
  // from by hand. A plain-HTTP origin has no clipboard API at all, and that is
  // a supported way to reach this portal (SSH-08).
  const copySelection = useCallback(
    async (text: string) => {
      if (await copyText(text)) {
        terminal.current?.clearSelection()
        flash('Copied.')
        return
      }
      setCopyPanel(text)
    },
    [flash],
  )

  // Selecting on the canvas stops working the moment the guest asks for mouse
  // events, which tmux and vim both do on sight. Lifting the buffer out as
  // text is the way to copy that no program on the far end can take away.
  const openSelect = useCallback((scope: SnapshotScope) => {
    setSelectScope(scope)
    setMouseGrabbed(terminal.current?.mouseReporting() ?? false)
    setSelectPanel(terminal.current?.snapshot(scope) ?? '')
  }, [])

  // Copy with nothing selected used to be a silent no-op. It is the button
  // somebody presses when a drag would not select, so it opens the panel that
  // makes selecting possible.
  const copyFromTerminal = useCallback(() => {
    const selected = terminal.current?.selection() ?? ''
    if (selected === '') openSelect('screen')
    else void copySelection(selected)
  }, [copySelection, openSelect])

  const pasteFromClipboard = useCallback(async () => {
    const text = await readText()
    if (text !== null && text !== '') {
      terminal.current?.paste(text)
      terminal.current?.focus()
      return
    }
    setPastePanel(true)
  }, [])

  const running = vm?.state === 'running'
  const viewportHeight = useViewportHeight()

  return (
    // 100dvh, then the measured visual viewport on top of it: the key bar is
    // the last row of this column, and on a phone both the browser's own bars
    // and the soft keyboard would otherwise leave it below the fold — see
    // useViewportHeight.
    <div
      className="flex h-[100dvh] flex-col bg-[#0b0f17] text-content"
      style={viewportHeight === null ? undefined : { height: viewportHeight }}
    >
      <header className="flex flex-wrap items-center gap-2 border-b border-border bg-surface px-3 py-2">
        <div className="mr-auto min-w-0">
          <h1 className="truncate text-sm font-medium">
            {vm?.name ?? 'SSH'}
            {session && (
              <span className="ml-2 font-mono text-xs text-muted">
                {session.ssh_user}@{session.address}
              </span>
            )}
          </h1>
          {vm && (
            <p className="truncate text-xs text-muted">
              {vm.platform_name}
              {vm.host_name ? ` · ${vm.host_name}` : ''} · {vm.state}
            </p>
          )}
        </div>

        {phase === 'connected' && (
          <div className="flex flex-wrap items-center gap-1">
            <ToolbarButton onClick={copyFromTerminal}>Copy</ToolbarButton>
            <ToolbarButton
              onClick={() => openSelect('screen')}
              pressed={selectPanel !== null}
              title="Select text to copy, including from tmux, vim or anything else holding the mouse"
            >
              Select
            </ToolbarButton>
            <ToolbarButton onClick={() => void pasteFromClipboard()}>Paste</ToolbarButton>
            {session?.files_available && (
              <ToolbarButton onClick={() => setFilesOpen((open) => !open)} pressed={filesOpen}>
                Files
              </ToolbarButton>
            )}
            <ToolbarButton onClick={() => setKeysOpen((open) => !open)} pressed={keysOpen}>
              Keys
            </ToolbarButton>
            <ToolbarButton
              onClick={() =>
                setFontSize((size) => FONT_SIZES[Math.max(0, FONT_SIZES.indexOf(size) - 1)])
              }
              aria-label="Smaller text"
            >
              A−
            </ToolbarButton>
            <ToolbarButton
              onClick={() =>
                setFontSize(
                  (size) =>
                    FONT_SIZES[Math.min(FONT_SIZES.length - 1, FONT_SIZES.indexOf(size) + 1)],
                )
              }
              aria-label="Larger text"
            >
              A+
            </ToolbarButton>
            <ToolbarButton onClick={() => terminal.current?.clear()}>Clear</ToolbarButton>
            {keyState?.key_exists === true &&
              (installedFor(session?.ssh_user ?? '') ? (
                <ToolbarButton onClick={() => void keyAction(false)} disabled={keyBusy}>
                  Remove portal key
                </ToolbarButton>
              ) : (
                <ToolbarButton onClick={() => void keyAction(true)} disabled={keyBusy}>
                  Install portal key
                </ToolbarButton>
              ))}
            <ToolbarButton onClick={() => void disconnect()} danger>
              Disconnect
            </ToolbarButton>
          </div>
        )}
      </header>

      <div className="relative flex min-h-0 flex-1">
        <main className="flex min-w-0 flex-1 flex-col">
          {phase === 'connected' && session ? (
            <>
              <div className="relative min-h-0 flex-1">
                <SshTerminal
                  ref={terminal}
                  wsUrl={session.ws_url}
                  fontSize={fontSize}
                  onOpen={() => flash('Connected.')}
                  onClose={(detail) => {
                    setPhase('closed')
                    setMessage(detail)
                  }}
                  onCopyRequest={(text) => void copySelection(text)}
                  onPasteRequest={() => void pasteFromClipboard()}
                  onModifiersChange={setModifiers}
                />
                {/* Over the terminal, never instead of it: unmounting the
                    terminal would close the socket and cost another password. */}
                {selectPanel !== null && (
                  <TerminalSelect
                    text={selectPanel}
                    scope={selectScope}
                    fontSize={fontSize}
                    mouseGrabbed={mouseGrabbed}
                    onScope={openSelect}
                    onCopy={(text) => void copySelection(text)}
                    onNotice={flash}
                    onClose={() => {
                      setSelectPanel(null)
                      terminal.current?.focus()
                    }}
                  />
                )}
              </div>
              {keysOpen && <TerminalKeys terminal={terminal} modifiers={modifiers} />}
            </>
          ) : (
            <div className="flex h-full items-start justify-center overflow-y-auto p-6">
              <div className="w-full max-w-md space-y-4">
                {phase === 'connecting' && (
                  <div className="flex items-center gap-3 text-sm text-muted">
                    <span className="h-4 w-4 animate-spin rounded-full border-2 border-muted/40 border-t-muted" />
                    Opening the connection…
                  </div>
                )}

                {mismatch && <HostKeyMismatch detail={mismatch} />}

                {prompt && (
                  <HostKeyConfirm
                    detail={prompt}
                    onAccept={() => void connect(prompt.fingerprint)}
                    onCancel={() => setPrompt(null)}
                  />
                )}

                {phase !== 'connecting' && !prompt && (
                  <form
                    className="space-y-3 rounded-lg border border-border bg-surface p-4"
                    onSubmit={(event) => {
                      event.preventDefault()
                      void connect()
                    }}
                  >
                    <h2 className="text-sm font-medium">
                      {phase === 'closed' ? 'Connect again' : 'Connect over SSH'}
                    </h2>
                    <p className="text-xs text-muted">
                      These credentials belong to the guest, not to the portal. They are used to
                      open this one connection and are never stored. The portal's own key is the
                      exception, and it is the portal's — installing it is how you stop typing a
                      password here.
                    </p>

                    {!running && vm && (
                      <p className="rounded-md border border-border px-3 py-2 text-xs text-muted">
                        This VM is {vm.state}. Nothing will be listening for SSH.
                      </p>
                    )}

                    <Field label="Username">
                      <input
                        value={username}
                        onChange={(event) => setUsername(event.target.value)}
                        autoComplete="off"
                        autoCapitalize="none"
                        spellCheck={false}
                        className="w-full rounded-md border border-border bg-surface-raised px-2 py-1.5 text-sm"
                      />
                    </Field>

                    <div className="flex gap-2 text-xs">
                      <Choice active={auth === 'password'} onClick={() => setAuth('password')}>
                        Password
                      </Choice>
                      <Choice active={auth === 'key'} onClick={() => setAuth('key')}>
                        Private key
                      </Choice>
                      {keyState?.key_exists === true && (
                        <Choice active={auth === 'portal'} onClick={() => setAuth('portal')}>
                          Portal key
                        </Choice>
                      )}
                    </div>

                    {auth === 'portal' ? (
                      <p className="rounded-md border border-border px-3 py-2 text-xs text-muted">
                        {keyReady
                          ? `The portal's key is installed for ${username} on this VM. No password needed.`
                          : `The portal's key is not installed for ${username} here yet. Connect with a password once, then use Install portal key.`}
                      </p>
                    ) : auth === 'password' ? (
                      <Field label="Password">
                        <input
                          type="password"
                          value={password}
                          onChange={(event) => setPassword(event.target.value)}
                          autoComplete="off"
                          className="w-full rounded-md border border-border bg-surface-raised px-2 py-1.5 text-sm"
                        />
                      </Field>
                    ) : (
                      <>
                        <Field label="Private key">
                          <textarea
                            value={privateKey}
                            onChange={(event) => setPrivateKey(event.target.value)}
                            rows={5}
                            spellCheck={false}
                            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
                            className="w-full rounded-md border border-border bg-surface-raised p-2 font-mono text-xs"
                          />
                        </Field>
                        <Field label="Key passphrase (if it has one)">
                          <input
                            type="password"
                            value={passphrase}
                            onChange={(event) => setPassphrase(event.target.value)}
                            autoComplete="off"
                            className="w-full rounded-md border border-border bg-surface-raised px-2 py-1.5 text-sm"
                          />
                        </Field>
                      </>
                    )}

                    <div className="flex gap-2">
                      <Field label="Address">
                        {/* Only addresses the platform reported: the portal is
                            not an SSH proxy to the rest of the network. */}
                        <select
                          value={host}
                          onChange={(event) => setHost(event.target.value)}
                          className="w-full rounded-md border border-border bg-surface-raised px-2 py-1.5 text-sm"
                        >
                          <option value="">Choose automatically</option>
                          {(vm?.ip_addresses ?? []).map((address) => (
                            <option key={address} value={address}>
                              {address}
                            </option>
                          ))}
                        </select>
                      </Field>
                      <Field label="Port">
                        <input
                          value={port}
                          onChange={(event) => setPort(event.target.value)}
                          inputMode="numeric"
                          className="w-20 rounded-md border border-border bg-surface-raised px-2 py-1.5 text-sm"
                        />
                      </Field>
                    </div>

                    {message !== '' && (
                      <p
                        className={`text-xs ${phase === 'error' ? 'text-danger' : 'text-muted'}`}
                        role="status"
                      >
                        {message}
                      </p>
                    )}

                    <button
                      type="submit"
                      className="w-full rounded-md bg-accent px-4 py-2 text-sm font-medium text-white disabled:opacity-50"
                      disabled={
                        username === '' ||
                        (auth === 'password' && password === '') ||
                        (auth === 'key' && privateKey === '')
                      }
                    >
                      Connect
                    </button>
                  </form>
                )}
              </div>
            </div>
          )}
        </main>

        {filesOpen && session?.files_available && (
          <FileBrowser
            sessionId={session.session_id}
            home={session.home}
            onClose={() => setFilesOpen(false)}
            onNotice={flash}
          />
        )}
      </div>

      {/* Paste panel: the only way in on an origin whose clipboard the browser
          will not read. Typing into a textarea needs no permission anywhere. */}
      {pastePanel && (
        <ClipboardPanel
          title="Paste into the terminal"
          hint={
            canReadClipboard()
              ? 'The browser would not hand over the clipboard. Paste here instead.'
              : 'This page is not on a secure origin, so the browser will not share the clipboard. Paste here instead.'
          }
          value={pasteText}
          onChange={setPasteText}
          confirmLabel="Send"
          onConfirm={() => {
            terminal.current?.paste(pasteText)
            setPasteText('')
            setPastePanel(false)
            terminal.current?.focus()
          }}
          onClose={() => setPastePanel(false)}
        />
      )}

      {copyPanel !== '' && (
        <ClipboardPanel
          title="Copy from the terminal"
          hint="The browser would not write to the clipboard. Select this text and copy it."
          value={copyPanel}
          readOnly
          confirmLabel="Done"
          onConfirm={() => setCopyPanel('')}
          onClose={() => setCopyPanel('')}
        />
      )}

      {phase === 'closed' && (
        <div className="absolute inset-x-0 bottom-0 border-t border-border bg-surface px-4 py-3 text-sm">
          <span className="text-muted">{message}</span>
          <button
            onClick={() => {
              setPhase('form')
              setSession(null)
            }}
            className="ml-3 rounded-md bg-accent px-3 py-1 text-xs font-medium text-white"
          >
            Connect again
          </button>
        </div>
      )}

      <p aria-live="polite" className="sr-only">
        {notice}
      </p>
      {notice !== '' && (
        <div className="pointer-events-none absolute bottom-4 left-1/2 z-30 -translate-x-1/2 rounded-md bg-surface px-3 py-1.5 text-xs shadow">
          {notice}
        </div>
      )}
    </div>
  )
}

function HostKeyConfirm({
  detail,
  onAccept,
  onCancel,
}: {
  detail: SshHostKeyPrompt
  onAccept: () => void
  onCancel: () => void
}) {
  return (
    <div className="space-y-3 rounded-lg border border-border bg-surface p-4">
      <h2 className="text-sm font-medium">Trust this machine?</h2>
      <p className="text-xs text-muted">
        The portal has not connected to {detail.address} before. Check that this fingerprint matches
        the one the machine reports — on the machine itself, <code>ssh-keyscan</code> or the console
        will tell you. Accepting pins it, and a later change will be refused.
      </p>
      <p className="break-all rounded-md border border-border bg-surface-raised p-2 font-mono text-xs">
        {detail.fingerprint}
        <span className="ml-2 text-muted">({detail.algorithm})</span>
      </p>
      <div className="flex gap-2">
        <button
          onClick={onAccept}
          className="rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-white"
        >
          Accept and connect
        </button>
        <button onClick={onCancel} className="rounded-md border border-border px-3 py-1.5 text-xs">
          Cancel
        </button>
      </div>
    </div>
  )
}

function HostKeyMismatch({ detail }: { detail: SshHostKeyMismatch }) {
  return (
    <div className="space-y-2 rounded-lg border border-danger/50 bg-surface p-4">
      <h2 className="text-sm font-medium text-danger">The host key has changed</h2>
      <p className="text-xs text-muted">
        {detail.address} is presenting a different key than the one trusted on{' '}
        {new Date(detail.first_seen_at).toLocaleDateString()}. If this guest was rebuilt, an
        administrator can forget the old key. If it was not, stop: something is answering in its
        place.
      </p>
      <dl className="space-y-1 font-mono text-[11px]">
        <div>
          <dt className="inline text-muted">expected </dt>
          <dd className="inline break-all">{detail.expected}</dd>
        </div>
        <div>
          <dt className="inline text-muted">received </dt>
          <dd className="inline break-all">{detail.got}</dd>
        </div>
      </dl>
    </div>
  )
}

function ClipboardPanel({
  title,
  hint,
  value,
  onChange,
  readOnly,
  confirmLabel,
  onConfirm,
  onClose,
}: {
  title: string
  hint: string
  value: string
  onChange?: (text: string) => void
  readOnly?: boolean
  confirmLabel: string
  onConfirm: () => void
  onClose: () => void
}) {
  const field = useRef<HTMLTextAreaElement>(null)
  useEffect(() => {
    field.current?.focus()
    if (readOnly) field.current?.select()
  }, [readOnly])

  return (
    <div className="absolute inset-x-0 bottom-0 z-20 border-t border-border bg-surface p-3">
      <div className="mx-auto max-w-2xl space-y-2">
        <div className="flex items-center justify-between">
          <h2 className="text-sm font-medium">{title}</h2>
          <button onClick={onClose} className="text-xs text-muted" aria-label="Close">
            ✕
          </button>
        </div>
        <p className="text-xs text-muted">{hint}</p>
        <textarea
          ref={field}
          aria-label={title}
          value={value}
          readOnly={readOnly}
          onChange={(event) => onChange?.(event.target.value)}
          rows={4}
          spellCheck={false}
          className="w-full rounded-md border border-border bg-surface-raised p-2 font-mono text-xs"
        />
        <button
          onClick={onConfirm}
          className="rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-white"
        >
          {confirmLabel}
        </button>
      </div>
    </div>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="block space-y-1">
      <span className="block text-xs font-medium text-muted">{label}</span>
      {children}
    </label>
  )
}

function Choice({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={`rounded-md border px-3 py-1 ${
        active ? 'border-accent bg-accent/10 text-content' : 'border-border text-muted'
      }`}
    >
      {children}
    </button>
  )
}

function ToolbarButton({
  children,
  onClick,
  pressed,
  danger,
  ...rest
}: React.ButtonHTMLAttributes<HTMLButtonElement> & { pressed?: boolean; danger?: boolean }) {
  return (
    <button
      onClick={onClick}
      aria-pressed={pressed}
      className={`rounded-md border px-2 py-1 text-xs ${
        danger
          ? 'border-danger/50 text-danger'
          : pressed
            ? 'border-accent bg-accent/10'
            : 'border-border hover:bg-surface-inset'
      }`}
      {...rest}
    >
      {children}
    </button>
  )
}
