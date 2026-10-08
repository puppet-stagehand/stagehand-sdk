// @stagehand/sdk-ui — slot contract, contract_version 1 (UI bundle contract, format 1).
//
// A pack's in-process UI is ONE native ES module (`entry` in ui.manifest.json)
// whose default export is a PackEntry. There is no module federation and no
// `definePackUI`: the build marks four modules external and rewrites them to
// same-origin shim URLs (see ShimUrls); the console serves the shims.
// Components are built ONLY from `@stagehand/console-ui` (theme tokens,
// primitives); packs never see tokens or credentials.
//
// Bundle layout, digest rule and limits: docs/ui-bundle-contract.md.
// Sandboxed tier (`sandbox_entry`): see sandbox.d.ts.

import type { ComponentType } from "react";

export type SlotKind =
  | "page"                 // renders at manifest.nav.route, inside the console shell
  | "settingsPanel"        // extra panel under the host-rendered settings form
  | "nodeDetailTab"
  | "deviceDetailTab"
  | "inventoryKindView"    // list + detail for a kind the pack registered
  | "complianceSourceCard"
  | "dashboardCard";

/** The format of ui.manifest.json this file describes. */
export type UIManifestFormat = 1;

/** Value of PackEntry.contract_version; also the `v1` in /runtime/v1/. */
export type EntryContractVersion = 1;

/**
 * Props every slot component receives. `expansionId` is the pack's manifest
 * id. A pack reaches its own routes at /api/v1/x/<expansionId>/ with the
 * session the console already holds; it never receives a token.
 */
export interface SlotProps {
  page: { expansionId: string; route?: string };
  settingsPanel: { expansionId: string };
  nodeDetailTab: { expansionId: string; certname: string };
  deviceDetailTab: { expansionId: string; deviceId: string };
  // The three below keep their pre-bundle-contract props. The console does not
  // yet deliver them (they are outside the first delivery's panel slots).
  inventoryKindView: { expansionId: string; kind?: string; itemId?: string };
  complianceSourceCard: { expansionId: string; sourceId?: string };
  dashboardCard: { expansionId: string };
}

/** Default export of the `entry` module. */
export interface PackEntry {
  contract_version: EntryContractVersion;
  slots: Partial<{ [K in SlotKind]: ComponentType<SlotProps[K]> }>;
}

/** Identity helper that type-checks an entry. Optional; a plain object works. */
export declare function definePackEntry(entry: PackEntry): PackEntry;

/** Modules a pack build must mark external, and the URL each is rewritten to. */
export interface ShimUrls {
  react: "/runtime/v1/react.js";
  "react/jsx-runtime": "/runtime/v1/jsx-runtime.js";
  "react-dom": "/runtime/v1/react-dom.js";
  "@stagehand/console-ui": "/runtime/v1/console-ui.js";
}

/** What /runtime/v1/react-dom.js re-exports. */
export type ReactDomShimExports = "createPortal" | "flushSync";

/** What /runtime/v1/console-ui.js re-exports. Adding one is additive; removing one breaks the contract. */
export type ConsoleUIPrimitive =
  | "Alert" | "Badge" | "Button" | "Card" | "Checkbox" | "Collapsible" | "Dialog"
  | "Drawer" | "HelpBubble" | "Input" | "LoadingState" | "Radio" | "SectionHeader"
  | "Select" | "Spinner" | "StatBlock" | "Switch" | "Tabs" | "Tag" | "Tooltip";

/** Status glyph + label; status is never conveyed by colour alone. */
export type StatusKind = "unchanged" | "changed" | "failed" | "unreported" | "overdue" | "pending" | "ok";
