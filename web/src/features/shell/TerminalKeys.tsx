import type { Modifiers, SpecialKey, TerminalHandle } from './SshTerminal'

/**
 * The keys a terminal needs and a touch keyboard does not have (SSH-08).
 *
 * The selection is not "every key" — it is the ones people actually reach for
 * in a shell, in the order they reach for them. Tab completes, the arrows
 * recall and edit a command, Ctrl+C stops whatever is running, and Esc is half
 * of using vim. On a phone none of them exist, which makes a browser terminal
 * something you can read output in but not work in.
 *
 * The modifiers are sticky rather than held: there is no chording on a
 * touchscreen, so Ctrl arms and the next keystroke spends it — from this bar
 * or from the on-screen keyboard, which is what makes Ctrl then C work.
 */

const KEYS: { key: SpecialKey; label: string; hint: string }[] = [
  { key: 'escape', label: 'Esc', hint: 'Escape' },
  { key: 'tab', label: 'Tab', hint: 'Tab — complete' },
  { key: 'left', label: '←', hint: 'Left' },
  { key: 'up', label: '↑', hint: 'Up — previous command' },
  { key: 'down', label: '↓', hint: 'Down — next command' },
  { key: 'right', label: '→', hint: 'Right' },
  { key: 'home', label: 'Home', hint: 'Start of line' },
  { key: 'end', label: 'End', hint: 'End of line' },
  { key: 'pageup', label: 'PgUp', hint: 'Page up' },
  { key: 'pagedown', label: 'PgDn', hint: 'Page down' },
]

// The combinations common enough that arming Ctrl first would be a tap too
// many. Each is one keystroke on a real keyboard and should be one here.
const SHORTCUTS: { letter: string; label: string; hint: string }[] = [
  { letter: 'c', label: '^C', hint: 'Interrupt whatever is running' },
  { letter: 'd', label: '^D', hint: 'End of input — log out' },
  { letter: 'z', label: '^Z', hint: 'Suspend to the background' },
  { letter: 'l', label: '^L', hint: 'Clear the screen' },
  { letter: 'r', label: '^R', hint: 'Search command history' },
]

export function TerminalKeys({
  terminal,
  modifiers,
}: {
  terminal: React.RefObject<TerminalHandle | null>
  /** Which modifiers are armed, so a tap is visibly a state and not a no-op. */
  modifiers: Modifiers
}) {
  return (
    <div
      // Horizontal scroll rather than wrapping: on the narrow screen this bar
      // exists for, wrapping would eat two rows of the terminal it is meant to
      // make usable.
      className="flex shrink-0 items-center gap-1 overflow-x-auto border-t border-border bg-surface px-2 py-1.5"
      role="group"
      aria-label="Terminal keys"
    >
      <Key
        label="Ctrl"
        hint="Ctrl — applies to the next key you press"
        pressed={modifiers.ctrl}
        onPress={() => terminal.current?.toggleModifier('ctrl')}
      />
      <Key
        label="Alt"
        hint="Alt — applies to the next key you press"
        pressed={modifiers.alt}
        onPress={() => terminal.current?.toggleModifier('alt')}
      />

      <span className="mx-1 h-5 w-px shrink-0 bg-border" aria-hidden="true" />

      {KEYS.map(({ key, label, hint }) => (
        <Key key={key} label={label} hint={hint} onPress={() => terminal.current?.press(key)} />
      ))}

      <span className="mx-1 h-5 w-px shrink-0 bg-border" aria-hidden="true" />

      {SHORTCUTS.map(({ letter, label, hint }) => (
        <Key
          key={letter}
          label={label}
          hint={hint}
          onPress={() => terminal.current?.control(letter)}
        />
      ))}
    </div>
  )
}

function Key({
  label,
  hint,
  pressed,
  onPress,
}: {
  label: string
  hint: string
  pressed?: boolean
  onPress: () => void
}) {
  return (
    <button
      type="button"
      title={hint}
      aria-label={hint}
      aria-pressed={pressed}
      // onMouseDown/onTouchStart would fire before the terminal loses focus,
      // but preventDefault on mousedown is the part that matters: without it
      // the button steals focus and the next real keystroke goes nowhere.
      onMouseDown={(event) => event.preventDefault()}
      onClick={onPress}
      className={`min-w-9 shrink-0 rounded-md border px-2 py-1.5 font-mono text-xs ${
        pressed
          ? 'border-accent bg-accent text-white'
          : 'border-border hover:bg-surface-inset active:bg-surface-raised'
      }`}
    >
      {label}
    </button>
  )
}
