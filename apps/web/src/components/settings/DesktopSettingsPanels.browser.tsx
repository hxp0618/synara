// FILE: DesktopSettingsPanels.browser.tsx
// Purpose: Lock the browser/native lifecycle behavior owned by the desktop settings panels.
// Layer: Browser UI test

import "../../index.css";

import type { DesktopAppSnapState, DesktopSaaSConnectionState } from "@synara/contracts";
import type { AppSettingsBinding } from "~/appSettings";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render } from "vitest-browser-react";

const harness = vi.hoisted(() => ({
  settings: {
    appSnapPlaySound: true,
    appSnapShortcut: { kind: "both-option-keys" as const },
    enableAppSnap: false,
    enableSystemTaskCompletionNotifications: false,
    enableTaskCompletionToasts: true,
  },
  defaults: {
    appSnapPlaySound: true,
    appSnapShortcut: { kind: "both-option-keys" as const },
    enableAppSnap: false,
    enableSystemTaskCompletionNotifications: false,
    enableTaskCompletionToasts: true,
  },
  updateSettings: vi.fn(),
  readBrowserPermission: vi.fn(() => "default"),
  requestBrowserPermission: vi.fn(),
  toastAdd: vi.fn(),
}));

vi.mock("~/env", () => ({ isElectron: false }));

vi.mock("~/notifications/taskCompletion", () => ({
  buildNotificationSettingsSupportText: (permission: string) => `Permission: ${permission}`,
  readBrowserNotificationPermissionState: harness.readBrowserPermission,
  requestBrowserNotificationPermission: harness.requestBrowserPermission,
}));

vi.mock("~/components/ui/toast", () => ({
  toastManager: { add: harness.toastAdd },
}));

import {
  AppSnapSettingsPanel,
  NotificationsSettingsPanel,
  SaaSConnectionSettingsPanel,
} from "./DesktopSettingsPanels";

const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

function settingsBinding(): AppSettingsBinding {
  return {
    settings: harness.settings,
    defaults: harness.defaults,
    updateSettings: harness.updateSettings,
  } as unknown as AppSettingsBinding;
}

function AppSnapActivityHarness() {
  const [active, setActive] = useState(true);
  return (
    <QueryClientProvider client={queryClient}>
      <button type="button" onClick={() => setActive(false)}>
        Leave AppSnap
      </button>
      <button type="button" onClick={() => setActive(true)}>
        Return to AppSnap
      </button>
      <AppSnapSettingsPanel active={active} {...settingsBinding()} />
    </QueryClientProvider>
  );
}

const READY_STATE: DesktopAppSnapState = {
  platform: "macos",
  supported: true,
  enabled: true,
  status: "ready",
  shortcut: { kind: "both-option-keys" },
  inputMonitoringPermission: "granted",
  screenRecordingPermission: "granted",
  message: null,
};

const CONNECTED_SAAS_STATE: DesktopSaaSConnectionState = {
  status: "connected",
  controlPlaneBaseUrl: "https://control.example.com/control-plane",
  deviceId: "device-1",
  userId: "user-1",
  email: "owner@example.com",
  displayName: "Owner",
  tenantId: "tenant-1",
  tenantName: "Northstar Labs",
  organizationId: "organization-1",
  organizationName: "Platform",
  credentialExpiresAt: "2026-09-01T00:00:00Z",
  message: null,
};

const DISCONNECTED_SAAS_STATE: DesktopSaaSConnectionState = {
  status: "disconnected",
  controlPlaneBaseUrl: null,
  deviceId: null,
  userId: null,
  email: null,
  displayName: null,
  tenantId: null,
  tenantName: null,
  organizationId: null,
  organizationName: null,
  credentialExpiresAt: null,
  message: null,
};

function setDesktopBridge(value: unknown): void {
  Object.defineProperty(window, "desktopBridge", {
    configurable: true,
    value,
  });
}

beforeEach(() => {
  harness.updateSettings.mockReset();
  harness.readBrowserPermission.mockReset().mockReturnValue("default");
  harness.requestBrowserPermission.mockReset();
  harness.toastAdd.mockReset();
  queryClient.clear();
  setDesktopBridge(undefined);
});

afterEach(() => {
  document.body.innerHTML = "";
  setDesktopBridge(undefined);
});

describe("NotificationsSettingsPanel", () => {
  it("keeps the preference disabled and explains a denied browser permission", async () => {
    harness.requestBrowserPermission.mockResolvedValue("denied");
    const mounted = await render(<NotificationsSettingsPanel active {...settingsBinding()} />);

    await mounted.getByLabelText("Desktop activity notifications").click();

    await vi.waitFor(() => {
      expect(harness.updateSettings).toHaveBeenCalledWith({
        enableSystemTaskCompletionNotifications: false,
      });
      expect(harness.toastAdd).toHaveBeenCalledWith(
        expect.objectContaining({
          type: "warning",
          title: "Desktop notifications unavailable",
        }),
      );
    });

    await mounted.unmount();
  });
});

describe("SaaSConnectionSettingsPanel", () => {
  it("shows only connection metadata and disconnects through the main-process bridge", async () => {
    const unsubscribe = vi.fn();
    const unsubscribeMode = vi.fn();
    const disconnectedState = {
      ...CONNECTED_SAAS_STATE,
      status: "disconnected" as const,
      controlPlaneBaseUrl: null,
      deviceId: null,
      userId: null,
      email: null,
      displayName: null,
      tenantId: null,
      tenantName: null,
      organizationId: null,
      organizationName: null,
      credentialExpiresAt: null,
    };
    const disconnect = vi.fn().mockResolvedValue(disconnectedState);
    const confirm = vi.fn().mockResolvedValue(true);
    setDesktopBridge({
      confirm,
      saas: {
        getMode: vi.fn().mockResolvedValue({ mode: "cloud", persisted: true }),
        setMode: vi.fn(),
        onMode: vi.fn(() => unsubscribeMode),
        getState: vi.fn().mockResolvedValue(CONNECTED_SAAS_STATE),
        disconnect,
        onState: vi.fn(() => unsubscribe),
      },
    });

    const mounted = await render(<SaaSConnectionSettingsPanel active />);

    await expect.element(mounted.getByText("owner@example.com")).toBeVisible();
    await expect.element(mounted.getByText("Northstar Labs")).toBeVisible();
    await expect.element(mounted.getByText("Platform")).toBeVisible();
    expect(document.body.textContent).not.toContain("device-1");

    await mounted.getByRole("button", { name: "Disconnect" }).click();
    await vi.waitFor(() => {
      expect(confirm).toHaveBeenCalledOnce();
      expect(disconnect).toHaveBeenCalledOnce();
      expect(harness.toastAdd).toHaveBeenCalledWith(
        expect.objectContaining({ title: "Desktop disconnected" }),
      );
    });

    await mounted.unmount();
    expect(unsubscribe).toHaveBeenCalledOnce();
    expect(unsubscribeMode).toHaveBeenCalledOnce();
  });

  it("switches to local mode without deleting the saved Cloud Panel connection", async () => {
    const setMode = vi.fn().mockResolvedValue({ mode: "local", persisted: true });
    const disconnect = vi.fn();
    setDesktopBridge({
      confirm: vi.fn(),
      saas: {
        getMode: vi.fn().mockResolvedValue({ mode: "cloud", persisted: true }),
        setMode,
        onMode: vi.fn(() => () => undefined),
        getState: vi.fn().mockResolvedValue(CONNECTED_SAAS_STATE),
        disconnect,
        onState: vi.fn(() => () => undefined),
      },
    });

    const mounted = await render(<SaaSConnectionSettingsPanel active />);
    await mounted.getByRole("button", { name: "Use local" }).click();

    await vi.waitFor(() => expect(setMode).toHaveBeenCalledWith("local"));
    expect(disconnect).not.toHaveBeenCalled();
    await mounted.unmount();
  });

  it("switches from local mode to Cloud Panel mode", async () => {
    const setMode = vi.fn().mockResolvedValue({ mode: "cloud", persisted: true });
    setDesktopBridge({
      confirm: vi.fn(),
      saas: {
        getMode: vi.fn().mockResolvedValue({ mode: "local", persisted: true }),
        setMode,
        onMode: vi.fn(() => () => undefined),
        getState: vi.fn().mockResolvedValue(DISCONNECTED_SAAS_STATE),
        disconnect: vi.fn(),
        onState: vi.fn(() => () => undefined),
      },
    });

    const mounted = await render(<SaaSConnectionSettingsPanel active />);
    await mounted.getByRole("button", { name: "Use Cloud Panel" }).click();

    await vi.waitFor(() => expect(setMode).toHaveBeenCalledWith("cloud"));
    await mounted.unmount();
  });

  it("explains that browser-only hosts cannot manage a device connection", async () => {
    const mounted = await render(<SaaSConnectionSettingsPanel active />);
    await expect
      .element(
        mounted.getByText(
          "Cloud Panel device connections are available in the installed Synara Desktop app.",
        ),
      )
      .toBeVisible();
    await expect.element(mounted.getByRole("button", { name: "Disconnect" })).toBeDisabled();
    await mounted.unmount();
  });
});

describe("AppSnapSettingsPanel", () => {
  it("owns the native state subscription and releases it on unmount", async () => {
    const unsubscribe = vi.fn();
    const getState = vi.fn().mockResolvedValue(READY_STATE);
    const requestPermissions = vi.fn().mockResolvedValue(READY_STATE);
    const setEnabled = vi.fn().mockResolvedValue(READY_STATE);
    const checkShortcut = vi.fn().mockResolvedValue({ available: true, reason: null });
    const setShortcut = vi.fn().mockResolvedValue({
      state: READY_STATE,
      availability: { available: true, reason: null },
    });
    const onState = vi.fn(() => unsubscribe);
    setDesktopBridge({
      appSnap: {
        getState,
        requestPermissions,
        setEnabled,
        checkShortcut,
        setShortcut,
        onState,
      },
    });

    const mounted = await render(<AppSnapActivityHarness />);
    await expect
      .element(mounted.getByText("Listening — press ⌥ left + ⌥ right to snap"))
      .toBeVisible();
    expect(onState).toHaveBeenCalledOnce();

    await mounted.getByRole("button", { name: "Leave AppSnap" }).click();
    await mounted.getByRole("button", { name: "Return to AppSnap" }).click();
    expect(onState).toHaveBeenCalledOnce();
    expect(unsubscribe).not.toHaveBeenCalled();

    await mounted.getByLabelText("Enable AppSnap").click();
    await vi.waitFor(() => {
      expect(requestPermissions).toHaveBeenCalledOnce();
      expect(setEnabled).toHaveBeenCalledWith(true);
      expect(harness.updateSettings).toHaveBeenCalledWith({ enableAppSnap: true });
    });

    await mounted.unmount();
    expect(unsubscribe).toHaveBeenCalledOnce();
  });

  it("records two keys, checks availability, and saves the shortcut", async () => {
    const checkShortcut = vi.fn().mockResolvedValue({ available: true, reason: null });
    const nextState = {
      ...READY_STATE,
      shortcut: { kind: "key-chord", modifier: "option", key: "KeyS" } as const,
    };
    const setShortcut = vi.fn().mockResolvedValue({
      state: nextState,
      availability: { available: true, reason: null },
    });
    setDesktopBridge({
      appSnap: {
        getState: vi.fn().mockResolvedValue(READY_STATE),
        requestPermissions: vi.fn().mockResolvedValue(READY_STATE),
        setEnabled: vi.fn().mockResolvedValue(READY_STATE),
        checkShortcut,
        setShortcut,
        onState: vi.fn(() => vi.fn()),
      },
    });

    const mounted = await render(<AppSnapActivityHarness />);
    await mounted.getByRole("button", { name: "Record AppSnap shortcut" }).click();
    const recorder = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Record AppSnap shortcut"]',
    );
    recorder?.dispatchEvent(
      new KeyboardEvent("keydown", {
        code: "AltLeft",
        key: "Alt",
        bubbles: true,
        cancelable: true,
      }),
    );
    recorder?.dispatchEvent(
      new KeyboardEvent("keydown", {
        code: "KeyS",
        key: "s",
        altKey: true,
        bubbles: true,
        cancelable: true,
      }),
    );

    await expect.element(mounted.getByText("Available — save to apply.")).toBeVisible();
    expect(checkShortcut).toHaveBeenCalledWith({
      kind: "key-chord",
      modifier: "option",
      key: "KeyS",
    });
    await mounted.getByRole("button", { name: "Save" }).click();
    await vi.waitFor(() => {
      expect(setShortcut).toHaveBeenCalledWith({
        kind: "key-chord",
        modifier: "option",
        key: "KeyS",
      });
      expect(harness.updateSettings).toHaveBeenCalledWith({
        appSnapShortcut: { kind: "key-chord", modifier: "option", key: "KeyS" },
      });
    });

    await mounted.unmount();
  });
});
