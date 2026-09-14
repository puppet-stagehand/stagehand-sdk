// @stagehand/sdk-ui — slot contract, contract_version 1.
// A pack's UI bundle exposes `./pack` → definePackUI({ id, slots }).
// Components are built ONLY from `@stagehand/console-ui` (primitives, theme
// tokens, useCapabilities, useLabsFeature, the typed API client that carries
// the session — packs never see tokens). Design gates run in pack-build.

import type { ComponentType } from "react";

export type SlotKind =
  | "page"                 // renders at manifest.nav.route, inside the console shell
  | "settingsPanel"        // extra panel under the host-rendered settings form
  | "nodeDetailTab"
  | "deviceDetailTab"
  | "inventoryKindView"    // list + detail for a kind the pack registered
  | "complianceSourceCard"
  | "dashboardCard";

export interface PackContext {
  packId: string;
  /** Typed client for the pack's own routes: /api/v1/x/{packId}/… */
  api: {
    request<T = unknown>(method: string, path: string, init?: { query?: Record<string, string>; body?: unknown }): Promise<T>;
  };
  /** Read-only view of the pack's validated settings. */
  settings: Record<string, unknown>;
  /** Console capability profile helpers. */
  capabilities: { enabled(id: string): boolean };
  /** Status glyph + label helper; status is never conveyed by colour alone. */
  status: (kind: "unchanged" | "changed" | "failed" | "unreported" | "overdue" | "pending" | "ok") => { glyph: string; label: string };
}

export interface SlotProps {
  page: { ctx: PackContext; route: string };
  settingsPanel: { ctx: PackContext };
  nodeDetailTab: { ctx: PackContext; certname: string };
  deviceDetailTab: { ctx: PackContext; deviceId: string };
  inventoryKindView: { ctx: PackContext; kind: string; itemId?: string };
  complianceSourceCard: { ctx: PackContext; sourceId: string };
  dashboardCard: { ctx: PackContext };
}

export interface PackUI {
  /** Must equal manifest.id */
  id: string;
  slots: Partial<{ [K in SlotKind]: ComponentType<SlotProps[K]> }>;
}

export declare function definePackUI(ui: PackUI): PackUI;

/** Names the console exposes in the module-federation shared scope. */
export type RuntimeSharedModules = "react" | "react-dom" | "@stagehand/console-ui" | "@stagehand/sdk-ui";
