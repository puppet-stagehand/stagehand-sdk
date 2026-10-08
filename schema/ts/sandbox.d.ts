// Sandboxed-tier contract (UI bundle contract, format 1).
//
// `sandbox_entry` in ui.manifest.json is ONE self-contained ES module: React is
// bundled in, there are no chunks and no externals. The console delivers it to
// a sandboxed frame over a private MessageChannel and the frame imports it
// from a blob URL. The frame has no console React to render with, so the
// default export mounts into an element with the bundle's own React.
//
// The sandbox is a different trust boundary from the in-process tier: the
// bundle cannot read the console's DOM, cookies or storage. Everything it
// needs comes through `host`.

import type { SlotKind, SlotProps } from "./slots";

/** Resolved token values. Names are the bridge's visual contract and may only grow. */
export type ThemeTokenName =
  // colour
  | "--content-bg" | "--card" | "--surface-2" | "--text" | "--muted" | "--border"
  | "--primary" | "--primary-fg" | "--danger" | "--danger-fg"
  | "--success" | "--warn" | "--info" | "--pending"
  // space
  | "--space-1" | "--space-2" | "--space-4" | "--space-5" | "--space-6" | "--space-8" | "--hit-target"
  // radius
  | "--radius-sm" | "--radius-md" | "--radius-pill"
  // type
  | "--font-brand" | "--font-mono" | "--fs-body" | "--fs-body-sm" | "--fs-h5"
  | "--fw-regular" | "--fw-semibold" | "--lh-body" | "--lh-heading" | "--lh-tight";

/** Resolved at send time from the console's computed styles; re-sent on theme change. */
export type ThemeTokens = Record<ThemeTokenName, string>;

export interface SandboxHost {
  /**
   * Proxied by the console. Restricted to /api/v1/x/<expansionId>/ — any other
   * path is refused. Resolves to the parsed JSON body; rejects on non-2xx.
   */
  api<T = unknown>(method: string, path: string, body?: unknown): Promise<T>;
  /** Ask the console to size the frame, in CSS pixels. */
  resize(height: number): void;
  /** Tell the console the pack handled Escape (or wants focus returned). */
  escape(): void;
  /** Subscribe to theme changes; fires once immediately. Returns an unsubscribe function. */
  onTheme(listener: (tokens: ThemeTokens) => void): () => void;
}

/** Renders a slot into `element` with the bundle's own React; returns an unmount function. */
export type SandboxMount<K extends SlotKind> = (
  element: HTMLElement,
  props: SlotProps[K],
  host: SandboxHost,
) => () => void;

/** Default export of the `sandbox_entry` module. */
export interface SandboxEntry {
  contract_version: 1;
  slots: Partial<{ [K in SlotKind]: SandboxMount<K> }>;
}
