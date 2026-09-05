import { describe, expect, it } from 'vitest'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join } from 'node:path'

/**
 * Every colour class in the source must name a colour the theme defines.
 *
 * Tailwind drops a class it does not recognise, silently and at build time, so
 * a typo or an invented name costs nothing until somebody looks at the screen.
 * That is how `bg-state-error text-white` shipped: the background vanished, the
 * white text stayed, and the button for confirming the destruction of a VM
 * rendered white on white. Nothing failed — the component tests query by role
 * and text, which is what they should do and exactly why they could not see it.
 *
 * The same mistake was already in the tree, and more widely: `bg-state-running`,
 * `text-state-stopped` and `bg-state-paused` appeared dozens of times, while the
 * colours built from those variables are named `running`, `stopped` and
 * `paused`. VM state badges had been rendering without their colours.
 */

// The colours declared in tailwind.config.ts, plus what Tailwind ships.
const themeColours = [
  'surface',
  'surface-raised',
  'border',
  'content',
  'muted',
  'accent',
  'running',
  'stopped',
  'paused',
  'stale',
  'warning',
  'danger',
]

const builtInColours = [
  'inherit',
  'current',
  'transparent',
  'black',
  'white',
  'slate',
  'gray',
  'zinc',
  'neutral',
  'stone',
  'red',
  'orange',
  'amber',
  'yellow',
  'lime',
  'green',
  'emerald',
  'teal',
  'cyan',
  'sky',
  'blue',
  'indigo',
  'violet',
  'purple',
  'fuchsia',
  'pink',
  'rose',
]

/**
 * Words that follow the same prefixes but are not colours: `text-sm` is a size,
 * `border-b` a side, `bg-cover` a fit. Saying what a colour is not is far
 * shorter than listing every utility that exists.
 */
const notColourNames = new Set([
  'xs',
  'sm',
  'base',
  'lg',
  'xl',
  '2xl',
  '3xl',
  '4xl',
  '5xl',
  '6xl',
  '7xl',
  '8xl',
  '9xl',
  't',
  'b',
  'l',
  'r',
  's',
  'e',
  'x',
  'y',
  'solid',
  'dashed',
  'dotted',
  'double',
  'hidden',
  'none',
  'collapse',
  'separate',
  'spacing',
  'left',
  'right',
  'center',
  'justify',
  'start',
  'end',
  'nowrap',
  'wrap',
  'balance',
  'pretty',
  'ellipsis',
  'clip',
  'cover',
  'contain',
  'repeat',
  'fixed',
  'local',
  'scroll',
  'gradient',
  'origin',
  'auto',
  'bottom',
  'top',
  'no',
  'inset',
  'offset',
  'opacity',
  'shown',
  'sr',
  'not',
])

/** A Tailwind utility, as it appears inside a class string. */
const utility =
  /(?:^|[\s"'`{])((?:bg|text|border|ring|fill|stroke|decoration|divide|outline|caret|placeholder|from|via|to)-[a-z][a-z0-9-]*(?:\/\d{1,3})?)(?=[\s"'`}]|$)/gm

function sourceFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((entry) => {
    const path = join(dir, entry)
    if (statSync(path).isDirectory()) return sourceFiles(path)
    return path.endsWith('.tsx') && !path.endsWith('.test.tsx') ? [path] : []
  })
}

/** The colour a utility names, or null when it names something else. */
function colourOf(className: string): string | null {
  const parts = className.split('/')[0].split('-').slice(1)
  // A trailing shade belongs to a built-in palette colour: red-500.
  if (parts.length > 0 && /^\d+$/.test(parts[parts.length - 1])) parts.pop()
  if (parts.length === 0) return null
  if (parts.some((part) => notColourNames.has(part))) return null
  return parts.join('-')
}

describe('theme colours', () => {
  it('are all names the theme actually defines', () => {
    const known = new Set([...themeColours, ...builtInColours])
    const unknown: string[] = []

    for (const file of sourceFiles('src')) {
      for (const match of readFileSync(file, 'utf8').matchAll(utility)) {
        const colour = colourOf(match[1])
        if (colour && !known.has(colour)) {
          unknown.push(`${file.replace('src/', '')}: ${match[1]}`)
        }
      }
    }

    expect(
      [...new Set(unknown)],
      'these name colours the theme does not define, so Tailwind drops them and the element ' +
        'renders unstyled — which is invisible until somebody looks at it:',
    ).toEqual([])
  })
})
