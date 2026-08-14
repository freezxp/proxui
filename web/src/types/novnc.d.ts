// noVNC ships no TypeScript declarations. This covers the surface the console
// page uses; extend it rather than reaching for `any` at the call site.
declare module '@novnc/novnc' {
  export interface RFBOptions {
    credentials?: { username?: string; password?: string; target?: string }
    shared?: boolean
    repeaterID?: string
    wsProtocols?: string[]
  }

  export default class RFB extends EventTarget {
    constructor(target: HTMLElement, urlOrChannel: string | WebSocket, options?: RFBOptions)
    viewOnly: boolean
    focusOnClick: boolean
    clipViewport: boolean
    dragViewport: boolean
    scaleViewport: boolean
    resizeSession: boolean
    showDotCursor: boolean
    background: string
    qualityLevel: number
    compressionLevel: number
    disconnect(): void
    focus(): void
    blur(): void
    sendCtrlAltDel(): void
    sendKey(keysym: number, code: string | null, down?: boolean): void
    clipboardPasteFrom(text: string): void
    machineShutdown(): void
    machineReboot(): void
  }
}
