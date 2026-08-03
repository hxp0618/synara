// FILE: SettingsSidebarNav.test.tsx
// Purpose: Guards the settings sidebar search surface and its ranking index.
// Layer: Component rendering tests
// Depends on: SettingsSidebarNav, the settings search index, and React server rendering.

import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";

import { SettingsSidebarNav } from "./SettingsSidebarNav";
import {
  CORE_SETTINGS_NAV_ITEMS,
  normalizeSettingsSection,
  settingRowAnchorId,
} from "../settingsNavigation";
import {
  SETTINGS_SEARCH_ENTRIES,
  rankSettingsSearchEntries,
  settingsSearchEntryTarget,
} from "../settingsSearchIndex";

describe("rankSettingsSearchEntries", () => {
  it("returns nothing for an empty query", () => {
    expect(rankSettingsSearchEntries("", 12)).toHaveLength(0);
    expect(rankSettingsSearchEntries("   ", 12)).toHaveLength(0);
  });

  it("ranks an exact title match first", () => {
    const [top] = rankSettingsSearchEntries("theme", 12);
    expect(top?.id).toBe("appearance:theme");
  });

  it("matches on description keywords, not just titles", () => {
    const results = rankSettingsSearchEntries("wrap", 12);
    expect(results.some((entry) => entry.id === "behavior:diff-line-wrapping")).toBe(true);
  });

  it("indexes the follow-up Queue and Steer preference", () => {
    const results = rankSettingsSearchEntries("steer", 12);
    expect(results.some((entry) => entry.id === "behavior:follow-up-behavior")).toBe(true);
  });

  it("includes the activity toasts notification row", () => {
    const results = rankSettingsSearchEntries("toasts", 12);
    expect(results.some((entry) => entry.id === "notifications:activity-toasts")).toBe(true);
  });

  it("indexes the registered enterprise destinations", () => {
    const results = rankSettingsSearchEntries("SCIM", 12);
    expect(results[0]?.section).toBe("organization-identity");
    expect(results[0]?.id).toBe("organization-identity:governance");
  });

  it("indexes environment instructions and the system UI font row", () => {
    expect(SETTINGS_SEARCH_ENTRIES.map((entry) => entry.id)).toEqual(
      expect.arrayContaining(["general:environment-instructions", "appearance:system-ui-font"]),
    );
  });

  it("surfaces every row in a section when searching the section label", () => {
    const results = rankSettingsSearchEntries("appearance", SETTINGS_SEARCH_ENTRIES.length);
    expect(results.some((entry) => entry.section === "appearance")).toBe(true);
  });

  it("respects the result limit", () => {
    expect(rankSettingsSearchEntries("e", 3)).toHaveLength(3);
  });

  it("derives a deep-link anchor target from each entry's title", () => {
    const themeEntry = SETTINGS_SEARCH_ENTRIES.find((entry) => entry.id === "appearance:theme")!;
    expect(settingsSearchEntryTarget(themeEntry)).toBe("setting-theme");
    for (const entry of SETTINGS_SEARCH_ENTRIES) {
      if (entry.target === null) {
        expect(settingsSearchEntryTarget(entry)).toBeNull();
      } else {
        expect(settingsSearchEntryTarget(entry)).toBe(settingRowAnchorId(entry.title));
        expect(settingsSearchEntryTarget(entry)?.startsWith("setting-")).toBe(true);
      }
    }
  });
});

describe("SettingsSidebarNav", () => {
  it("renders the soft search input alongside the section list", () => {
    const markup = renderToStaticMarkup(
      <SettingsSidebarNav activeSection="general" onBack={vi.fn()} onSelectSection={vi.fn()} />,
    );

    expect(markup).toContain('aria-label="Search settings"');
    expect(markup).toContain('aria-label="Settings sections"');
    expect(markup).toContain("Back to app");
  });

  it("groups settings by user intent instead of implementation ownership", () => {
    const markup = renderToStaticMarkup(
      <SettingsSidebarNav activeSection="general" onBack={vi.fn()} onSelectSection={vi.fn()} />,
    );

    expect(markup).toContain("Personal");
    expect(markup).toContain("Integrations");
    expect(markup).toContain("Coding");
    expect(markup).toContain("System");
    expect(markup).toContain("Archived");
    expect(markup).toContain("Chat behavior");
    expect(markup).toContain("MCP connections");
    expect(markup).toContain("Agent providers");
    expect(markup).toContain("Managed worktrees");
    expect(markup).toContain("System tools");
    expect(markup).toContain("Archived threads");
    expect(markup).not.toContain(">App<");
    expect(markup).not.toContain(">Synara<");
  });

  it("renders only host-resolved destinations", () => {
    const markup = renderToStaticMarkup(
      <SettingsSidebarNav
        activeSection="general"
        items={CORE_SETTINGS_NAV_ITEMS}
        onBack={vi.fn()}
        onSelectSection={vi.fn()}
      />,
    );

    expect(markup).not.toContain("Organization overview");
    expect(markup).not.toContain(">Organization<");
  });

  it("preserves the original Organization deep link as an overview alias", () => {
    expect(normalizeSettingsSection("tenancy")).toBe("organization-overview");
  });
});
