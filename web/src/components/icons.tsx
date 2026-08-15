/**
 * The navigation icon set.
 *
 * Hand-written rather than pulled from an icon package: ten glyphs is not
 * worth a dependency, and every icon library ships hundreds that tree-shaking
 * has to be trusted to remove — against a 1 MB budget for the initial bundle
 * (NFR-P5) that is a risk taken for nothing. These cost about 2 KB in total.
 *
 * All of them draw in `currentColor`, so an icon takes the colour of whatever
 * it sits in and the active-link and hover states need no icon-specific rules.
 * They are decorative: every caller states the label in text, visible or
 * screen-reader only, so the icons are hidden from assistive technology.
 */
import type { SVGProps } from 'react'

type IconProps = SVGProps<SVGSVGElement>

export type Icon = (props: IconProps) => JSX.Element

function Glyph({ children, ...props }: IconProps) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.75}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
      {...props}
    >
      {children}
    </svg>
  )
}

export const IconDashboard: Icon = (props) => (
  <Glyph {...props}>
    <rect x="3" y="3" width="7" height="7" rx="1.5" />
    <rect x="14" y="3" width="7" height="7" rx="1.5" />
    <rect x="14" y="14" width="7" height="7" rx="1.5" />
    <rect x="3" y="14" width="7" height="7" rx="1.5" />
  </Glyph>
)

export const IconInventory: Icon = (props) => (
  <Glyph {...props}>
    <path d="M12 2 2 7l10 5 10-5-10-5Z" />
    <path d="m2 12 10 5 10-5" />
    <path d="m2 17 10 5 10-5" />
  </Glyph>
)

export const IconHosts: Icon = (props) => (
  <Glyph {...props}>
    <rect x="2.5" y="3" width="19" height="7" rx="2" />
    <rect x="2.5" y="14" width="19" height="7" rx="2" />
    <path d="M6.5 6.5h.01M6.5 17.5h.01" />
  </Glyph>
)

export const IconStorage: Icon = (props) => (
  <Glyph {...props}>
    <ellipse cx="12" cy="5.5" rx="8.5" ry="3" />
    <path d="M3.5 5.5v13c0 1.66 3.8 3 8.5 3s8.5-1.34 8.5-3v-13" />
    <path d="M3.5 12c0 1.66 3.8 3 8.5 3s8.5-1.34 8.5-3" />
  </Glyph>
)

export const IconNetworks: Icon = (props) => (
  <Glyph {...props}>
    <circle cx="18" cy="5" r="2.75" />
    <circle cx="6" cy="12" r="2.75" />
    <circle cx="18" cy="19" r="2.75" />
    <path d="m8.4 13.4 7.2 4.2M15.6 6.4l-7.2 4.2" />
  </Glyph>
)

export const IconAudit: Icon = (props) => (
  <Glyph {...props}>
    <path d="M14 2H6.5A2.5 2.5 0 0 0 4 4.5v15A2.5 2.5 0 0 0 6.5 22h11a2.5 2.5 0 0 0 2.5-2.5V8Z" />
    <path d="M14 2v6h6" />
    <path d="M8.5 13h7M8.5 17h4.5" />
  </Glyph>
)

export const IconPlatforms: Icon = (props) => (
  <Glyph {...props}>
    <path d="M17.5 19.5a4.5 4.5 0 0 0 .48-8.97A6.001 6.001 0 0 0 6.2 9.7a4.5 4.5 0 0 0 .3 9.8Z" />
    <path d="M12 12v4.5" />
  </Glyph>
)

export const IconNotifications: Icon = (props) => (
  <Glyph {...props}>
    <path d="M18 8.5a6 6 0 1 0-12 0c0 6.5-2.5 8.5-2.5 8.5h17S18 15 18 8.5" />
    <path d="M13.7 20.5a2 2 0 0 1-3.4 0" />
  </Glyph>
)

export const IconUsers: Icon = (props) => (
  <Glyph {...props}>
    <path d="M15.5 21v-1.8a3.7 3.7 0 0 0-3.7-3.7H6.2a3.7 3.7 0 0 0-3.7 3.7V21" />
    <circle cx="9" cy="7.5" r="3.7" />
    <path d="M21.5 21v-1.8a3.7 3.7 0 0 0-2.8-3.58" />
    <path d="M15.8 4.02a3.7 3.7 0 0 1 0 7.16" />
  </Glyph>
)

export const IconSettings: Icon = (props) => (
  <Glyph {...props}>
    <circle cx="12" cy="12" r="3" />
    <path d="M19.14 12.94a1.5 1.5 0 0 1 0-1.88l1.2-1.5-1.7-2.94-1.85.6a1.5 1.5 0 0 1-1.63-.94L14.5 4.4h-3.4l-.66 1.88a1.5 1.5 0 0 1-1.63.94l-1.85-.6-1.7 2.94 1.2 1.5a1.5 1.5 0 0 1 0 1.88l-1.2 1.5 1.7 2.94 1.85-.6a1.5 1.5 0 0 1 1.63.94l.66 1.88h3.4l.66-1.88a1.5 1.5 0 0 1 1.63-.94l1.85.6 1.7-2.94Z" />
  </Glyph>
)

export const IconPublishing: Icon = (props) => (
  <Glyph {...props}>
    {/* A globe with a link through it: something private, reachable from
        outside. */}
    <circle cx="12" cy="12" r="9" />
    <path d="M3.5 9h17M3.5 15h17" />
    <path d="M12 3a14 14 0 0 1 0 18a14 14 0 0 1 0-18" />
  </Glyph>
)

/** The sidebar's collapse control. Rotated 180° to point the other way. */
export const IconChevronLeft: Icon = (props) => (
  <Glyph {...props}>
    <path d="m14.5 18-6-6 6-6" />
  </Glyph>
)
