// FILE: DesktopSettingsPanels.tsx
// Purpose: Own settings panels whose behavior depends on browser or desktop-native lifecycles.
// Layer: Settings UI components
// Exports: NotificationsSettingsPanel, AppSnapSettingsPanel, SaaSConnectionSettingsPanel

import {
  type DesktopAppSnapPermission,
  type DesktopAppSnapState,
  type DesktopConnectionModeState,
  type DesktopSaaSConnectionState,
  type ResolvedKeybindingsConfig,
} from "@synara/contracts";
import { appSnapShortcutLabels } from "@synara/shared/appSnapShortcut";
import { useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import type { AppSettingsBinding } from "~/appSettings";
import { createLatestAppSnapRequestGuard } from "~/appSnap.logic";
import { playAppSnapCaptureSound } from "~/lib/appSnapSound";
import { CentralIcon } from "~/lib/central-icons";
import { cn } from "~/lib/utils";
import { isElectron } from "~/env";
import {
  buildNotificationSettingsSupportText,
  readBrowserNotificationPermissionState,
  requestBrowserNotificationPermission,
} from "~/notifications/taskCompletion";
import {
  SETTINGS_CARD_ROW_DESCRIPTION_CLASS_NAME,
  SETTINGS_CARD_ROW_TITLE_CLASS_NAME,
} from "~/settingsPanelStyles";
import { Button } from "~/components/ui/button";
import { Switch } from "~/components/ui/switch";
import { toastManager } from "~/components/ui/toast";
import { serverConfigQueryOptions } from "~/lib/serverReactQuery";
import { AppSnapShortcutControl } from "./AppSnapShortcutControl";
import { SettingResetButton } from "./SettingControls";
import { SettingsCard, SettingsRow, SettingsSection } from "./SettingsPanelPrimitives";

const EMPTY_SAAS_CONNECTION_STATE: DesktopSaaSConnectionState = {
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

const EMPTY_DESKTOP_CONNECTION_MODE_STATE: DesktopConnectionModeState = {
  mode: "unselected",
  persisted: false,
};

function desktopSaaSStatusLabel(state: DesktopSaaSConnectionState): string {
  switch (state.status) {
    case "connected":
      return "Connected";
    case "connecting":
      return "Connecting…";
    case "disconnecting":
      return "Disconnecting…";
    case "error":
      return "Needs attention";
    default:
      return "Not connected";
  }
}

function credentialExpiryLabel(value: string | null): string {
  if (!value) return "—";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "—";
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

export function SaaSConnectionSettingsPanel({ active }: { readonly active: boolean }) {
  const [state, setState] = useState<DesktopSaaSConnectionState>(EMPTY_SAAS_CONNECTION_STATE);
  const [modeState, setModeState] = useState<DesktopConnectionModeState>(
    EMPTY_DESKTOP_CONNECTION_MODE_STATE,
  );
  const [modeBusy, setModeBusy] = useState(false);
  const [bridgeAvailable, setBridgeAvailable] = useState(false);

  useEffect(() => {
    const bridge = window.desktopBridge?.saas;
    setBridgeAvailable(Boolean(bridge));
    if (!bridge) return;
    let disposed = false;
    const unsubscribe = bridge.onState((nextState) => {
      if (!disposed) setState(nextState);
    });
    const unsubscribeMode = bridge.onMode((nextState) => {
      if (!disposed) setModeState(nextState);
    });
    void bridge
      .getState()
      .then((nextState) => {
        if (!disposed) setState(nextState);
      })
      .catch(() => {
        if (!disposed) {
          setState({
            ...EMPTY_SAAS_CONNECTION_STATE,
            status: "error",
            message: "Desktop connection state could not be loaded.",
          });
        }
      });
    void bridge
      .getMode()
      .then((nextState) => {
        if (!disposed) setModeState(nextState);
      })
      .catch(() => {
        if (!disposed) setModeState(EMPTY_DESKTOP_CONNECTION_MODE_STATE);
      });
    return () => {
      disposed = true;
      unsubscribe();
      unsubscribeMode();
    };
  }, []);

  async function selectMode(mode: "local" | "cloud") {
    const bridge = window.desktopBridge?.saas;
    if (!bridge || modeBusy || modeState.mode === mode) return;
    setModeBusy(true);
    try {
      const next = await bridge.setMode(mode);
      setModeState(next);
      toastManager.add({
        type: "success",
        title: mode === "local" ? "Using Synara locally" : "Using Cloud Panel mode",
        description:
          mode === "local"
            ? "Cloud Panel credentials, if present, remain protected for a later switch."
            : "A saved Cloud Panel connection will be restored; otherwise open an administrator-provided connection link.",
      });
    } catch (error) {
      toastManager.add({
        type: "error",
        title: "Could not switch Desktop mode",
        description: error instanceof Error ? error.message : "Desktop mode could not be changed.",
      });
    } finally {
      setModeBusy(false);
    }
  }

  async function disconnect() {
    const bridge = window.desktopBridge?.saas;
    if (!bridge || state.status !== "connected") return;
    const confirmed = await window.desktopBridge?.confirm(
      "Disconnect this device from Cloud Panel?\n\nLocal projects and chats stay on this device. The Desktop credential and device key will be removed.",
    );
    if (!confirmed) return;
    try {
      setState((current) => ({ ...current, status: "disconnecting", message: null }));
      const nextState = await bridge.disconnect();
      setState(nextState);
      toastManager.add({
        type: nextState.message ? "warning" : "success",
        title: "Desktop disconnected",
        description: nextState.message ?? "This device is no longer connected to Cloud Panel.",
      });
    } catch (error) {
      toastManager.add({
        type: "error",
        title: "Could not disconnect Desktop",
        description:
          error instanceof Error ? error.message : "The Desktop connection could not be removed.",
      });
      void bridge
        .getState()
        .then(setState)
        .catch(() => undefined);
    }
  }

  if (!active) return null;

  const connected = state.status === "connected";
  const cloudActive = modeState.mode === "cloud";
  const busy = modeBusy || state.status === "connecting" || state.status === "disconnecting";

  return (
    <div className="space-y-6">
      <SettingsCard divided={false} className="flex items-start gap-3 px-4 py-3.5">
        <span className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg border border-[color:var(--color-border)] text-muted-foreground">
          <CentralIcon name="cloud" className="size-4" />
        </span>
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <p className={SETTINGS_CARD_ROW_TITLE_CLASS_NAME}>Desktop operating mode</p>
            <span
              className={cn(
                "inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium",
                cloudActive && connected
                  ? "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
                  : state.status === "error"
                    ? "border-red-500/30 bg-red-500/10 text-red-700 dark:text-red-300"
                    : "border-[color:var(--color-border)] text-muted-foreground",
              )}
            >
              {cloudActive ? desktopSaaSStatusLabel(state) : "Local"}
            </span>
          </div>
          <p className={SETTINGS_CARD_ROW_DESCRIPTION_CLASS_NAME}>
            {bridgeAvailable
              ? cloudActive
                ? "Cloud Panel mode uses a protected device connection. If no account is saved, open a short-lived link generated by your Synara administrator."
                : "Local mode keeps projects and sessions on this device. A saved Cloud Panel connection is retained until you explicitly disconnect it."
              : "Cloud Panel device connections are available in the installed Synara Desktop app."}
          </p>
          {state.message ? (
            <p className="pt-1 text-xs leading-relaxed text-muted-foreground">{state.message}</p>
          ) : null}
        </div>
      </SettingsCard>

      <SettingsSection title="Operating mode">
        <SettingsRow
          title="Local"
          description="Run against the local Synara backend and keep Cloud Panel traffic disabled."
          status={modeState.mode === "local" ? "Current" : undefined}
          control={
            <Button
              size="xs"
              variant={modeState.mode === "local" ? "secondary" : "outline"}
              disabled={!bridgeAvailable || busy || modeState.mode === "local"}
              onClick={() => void selectMode("local")}
            >
              Use local
            </Button>
          }
        />
        <SettingsRow
          title="Cloud Panel"
          description="Restore the saved Cloud Panel connection, or wait for an administrator-provided Enrollment link."
          status={modeState.mode === "cloud" ? "Current" : undefined}
          control={
            <Button
              size="xs"
              variant={modeState.mode === "cloud" ? "secondary" : "outline"}
              disabled={!bridgeAvailable || busy || modeState.mode === "cloud"}
              onClick={() => void selectMode("cloud")}
            >
              Use Cloud Panel
            </Button>
          }
        />
      </SettingsSection>

      <SettingsSection title="Connection">
        <SettingsRow
          title="Account"
          description="The user identity bound to this device."
          status={connected ? state.displayName : undefined}
          control={
            <span className="max-w-64 truncate text-sm text-foreground">
              {connected ? state.email : "—"}
            </span>
          }
        />
        <SettingsRow
          title="Tenant"
          description="The active customer Tenant selected when this device was enrolled."
          control={
            <span className="max-w-64 truncate text-sm text-foreground">
              {connected ? state.tenantName : "—"}
            </span>
          }
        />
        <SettingsRow
          title="Organization"
          description="Optional Organization scope attached to the Enrollment."
          control={
            <span className="max-w-64 truncate text-sm text-foreground">
              {connected ? (state.organizationName ?? "Tenant-wide") : "—"}
            </span>
          }
        />
        <SettingsRow
          title="Control Plane"
          description="The allowlisted Cloud Panel endpoint used by this Desktop."
          control={
            <span className="max-w-64 truncate text-sm text-foreground">
              {connected ? state.controlPlaneBaseUrl : "—"}
            </span>
          }
        />
        <SettingsRow
          title="Credential rotation"
          description="Desktop rotates its protected device session before it expires."
          control={
            <span className="text-sm text-foreground">
              {connected ? credentialExpiryLabel(state.credentialExpiresAt) : "—"}
            </span>
          }
        />
      </SettingsSection>

      <SettingsSection title="This device">
        <SettingsRow
          title="Forget Cloud Panel connection"
          description="Revokes this device when the Control Plane is reachable, then removes its protected Cloud credential and key. Switching to local mode alone does not delete them."
          control={
            <Button
              size="xs"
              variant="outline"
              className="text-destructive hover:text-destructive"
              disabled={!connected || busy}
              onClick={() => void disconnect()}
            >
              {state.status === "disconnecting" ? "Disconnecting…" : "Disconnect"}
            </Button>
          }
        />
      </SettingsSection>
    </div>
  );
}

function appSnapStatusText(state: DesktopAppSnapState | null): string {
  if (!state) return "Available in the Synara desktop app";
  if (!state.supported) return state.message ?? "Available on macOS only";
  if (state.status === "ready") {
    const shortcut = state.shortcut;
    const label = shortcut ? appSnapShortcutLabels(shortcut).join(" + ") : "the shortcut";
    return `Listening — press ${label} to snap`;
  }
  if (state.status === "disabled") return "Off";
  if (state.status === "starting") return "Starting the capture listener…";
  return state.message ?? "Permission setup required";
}

const EMPTY_KEYBINDINGS: ResolvedKeybindingsConfig = [];

const APPSNAP_PERMISSION_LABELS: Record<DesktopAppSnapPermission, string> = {
  granted: "Granted",
  denied: "Denied",
  "not-determined": "Not requested yet",
  restricted: "Restricted",
  unknown: "Unknown",
};

function AppSnapPermissionBadge({ permission }: { permission: DesktopAppSnapPermission }) {
  return (
    <span className="inline-flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
      <span
        aria-hidden
        className={cn(
          "size-1.5 rounded-full",
          permission === "granted"
            ? "bg-emerald-500"
            : permission === "denied" || permission === "restricted"
              ? "bg-red-500"
              : "bg-[color:var(--color-border)]",
        )}
      />
      {APPSNAP_PERMISSION_LABELS[permission]}
    </span>
  );
}

export function NotificationsSettingsPanel({
  settings,
  defaults,
  updateSettings,
  active,
}: AppSettingsBinding & { readonly active: boolean }) {
  const [browserNotificationPermission, setBrowserNotificationPermission] = useState(
    readBrowserNotificationPermissionState(),
  );

  useEffect(() => {
    const timeoutId = window.setTimeout(() => {
      setBrowserNotificationPermission(readBrowserNotificationPermissionState());
    }, 0);
    return () => window.clearTimeout(timeoutId);
  }, []);

  async function setSystemNotificationsEnabled(nextEnabled: boolean) {
    if (!nextEnabled) {
      updateSettings({ enableSystemTaskCompletionNotifications: false });
      return;
    }

    if (isElectron) {
      updateSettings({ enableSystemTaskCompletionNotifications: true });
      return;
    }

    const permission = await requestBrowserNotificationPermission();
    setBrowserNotificationPermission(permission);

    if (permission === "granted") {
      updateSettings({ enableSystemTaskCompletionNotifications: true });
      return;
    }

    updateSettings({ enableSystemTaskCompletionNotifications: false });
    toastManager.add({
      type: permission === "denied" ? "warning" : "error",
      title: "Desktop notifications unavailable",
      description: buildNotificationSettingsSupportText(permission),
    });
  }

  async function sendTestNotification() {
    const title = "Activity notification";
    const body = "Notification test for chats and terminal agents.";

    if (window.desktopBridge) {
      const shown = await window.desktopBridge.notifications.show({ title, body, silent: false });
      toastManager.add({
        type: shown ? "success" : "warning",
        title: shown ? "Test notification sent" : "Notifications unavailable",
        description: shown
          ? "Your operating system should show the notification."
          : "Desktop notifications are not supported on this device.",
      });
      return;
    }

    const permission = await requestBrowserNotificationPermission();
    setBrowserNotificationPermission(permission);
    if (permission !== "granted") {
      toastManager.add({
        type: permission === "denied" ? "warning" : "error",
        title: "Desktop notifications unavailable",
        description: buildNotificationSettingsSupportText(permission),
      });
      return;
    }

    const notification = new Notification(title, { body, tag: "synara:test-notification" });
    notification.addEventListener("click", () => {
      window.focus();
    });
    toastManager.add({
      type: "success",
      title: "Test notification sent",
      description: "Your browser should show the notification.",
    });
  }

  if (!active) return null;

  return (
    <div className="space-y-6">
      <SettingsSection title="Activity alerts">
        <SettingsRow
          title="Activity toasts"
          description="Show an in-app toast when a chat or managed terminal agent finishes or needs input."
          resetAction={
            settings.enableTaskCompletionToasts !== defaults.enableTaskCompletionToasts ? (
              <SettingResetButton
                label="activity toasts"
                onClick={() =>
                  updateSettings({
                    enableTaskCompletionToasts: defaults.enableTaskCompletionToasts,
                  })
                }
              />
            ) : null
          }
          control={
            <Switch
              checked={settings.enableTaskCompletionToasts}
              onCheckedChange={(checked) =>
                updateSettings({ enableTaskCompletionToasts: Boolean(checked) })
              }
              aria-label="Activity toast notifications"
            />
          }
        />

        <SettingsRow
          title="Desktop notifications"
          description="Show an OS notification when a chat or managed terminal agent finishes or needs input while the app is in the background."
          status={buildNotificationSettingsSupportText(browserNotificationPermission)}
          resetAction={
            settings.enableSystemTaskCompletionNotifications !==
            defaults.enableSystemTaskCompletionNotifications ? (
              <SettingResetButton
                label="desktop notifications"
                onClick={() =>
                  updateSettings({
                    enableSystemTaskCompletionNotifications:
                      defaults.enableSystemTaskCompletionNotifications,
                  })
                }
              />
            ) : null
          }
          control={
            <div className="flex w-full items-center gap-2 sm:w-auto sm:justify-end">
              <Button size="xs" variant="outline" onClick={() => void sendTestNotification()}>
                Test
              </Button>
              <Switch
                checked={settings.enableSystemTaskCompletionNotifications}
                onCheckedChange={(checked) => {
                  void setSystemNotificationsEnabled(Boolean(checked));
                }}
                aria-label="Desktop activity notifications"
              />
            </div>
          }
        />
      </SettingsSection>
    </div>
  );
}

export function AppSnapSettingsPanel({
  settings,
  defaults,
  updateSettings,
  active,
}: AppSettingsBinding & { readonly active: boolean }) {
  const [appSnapState, setAppSnapState] = useState<DesktopAppSnapState | null>(null);
  const appSnapRequestGuardRef = useRef(createLatestAppSnapRequestGuard());
  const serverConfigQuery = useQuery({ ...serverConfigQueryOptions(), enabled: active });
  const keybindings = serverConfigQuery.data?.keybindings ?? EMPTY_KEYBINDINGS;

  useEffect(() => {
    const bridge = window.desktopBridge?.appSnap;
    if (!bridge) return;
    let disposed = false;
    const unsubscribe = bridge.onState((state) => {
      if (!disposed) setAppSnapState(state);
    });
    void bridge
      .getState()
      .then((state) => {
        if (!disposed) setAppSnapState(state);
      })
      .catch(() => undefined);
    return () => {
      disposed = true;
      unsubscribe();
    };
  }, []);

  async function setAppSnapEnabled(nextEnabled: boolean) {
    const requestGuard = appSnapRequestGuardRef.current;
    const requestId = requestGuard.begin();
    const bridge = window.desktopBridge?.appSnap;
    if (!bridge) {
      toastManager.add({
        type: "warning",
        title: "AppSnap unavailable",
        description: "AppSnap requires the Synara desktop app on macOS.",
      });
      return;
    }

    try {
      if (nextEnabled) {
        const permissionState = await bridge.requestPermissions();
        if (!requestGuard.isCurrent(requestId)) return;
        setAppSnapState(permissionState);
      }
      if (!requestGuard.isCurrent(requestId)) return;
      updateSettings({ enableAppSnap: nextEnabled });
      const state = await bridge.setEnabled(nextEnabled);
      if (!requestGuard.isCurrent(requestId)) return;
      setAppSnapState(state);
      if (nextEnabled && (state.status === "permission-required" || state.status === "error")) {
        toastManager.add({
          type: "warning",
          title: "Finish AppSnap setup",
          description: state.message ?? "Allow the required macOS permissions, then try again.",
        });
      }
    } catch (error) {
      if (!requestGuard.isCurrent(requestId)) return;
      updateSettings({ enableAppSnap: false });
      toastManager.add({
        type: "error",
        title: "AppSnap setup failed",
        description: error instanceof Error ? error.message : "Could not configure AppSnap.",
      });
    }
  }

  async function recheckAppSnapPermissions() {
    const bridge = window.desktopBridge?.appSnap;
    if (!bridge) return;
    const requestGuard = appSnapRequestGuardRef.current;
    const requestId = requestGuard.begin();
    try {
      await bridge.requestPermissions();
      const state = await bridge.setEnabled(settings.enableAppSnap);
      if (!requestGuard.isCurrent(requestId)) return;
      setAppSnapState(state);
    } catch (error) {
      if (!requestGuard.isCurrent(requestId)) return;
      toastManager.add({
        type: "error",
        title: "Could not check AppSnap permissions",
        description: error instanceof Error ? error.message : "Permission check failed.",
      });
    }
  }

  const supported = appSnapState?.supported === true;
  const enabled = supported && settings.enableAppSnap;

  if (!active) return null;

  return (
    <div className="space-y-6">
      <SettingsCard divided={false} className="flex items-start gap-3 px-4 py-3.5">
        <span className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg border border-[color:var(--color-border)] text-muted-foreground">
          <CentralIcon name="screen-capture" className="size-4" />
        </span>
        <div className="min-w-0 space-y-1">
          <p className={SETTINGS_CARD_ROW_TITLE_CLASS_NAME}>
            Take an AppSnap to show your agent another app's window
          </p>
          <p className={SETTINGS_CARD_ROW_DESCRIPTION_CLASS_NAME}>
            Press your two-key shortcut while any app is frontmost. Synara captures that window as
            an image, brings itself forward, and attaches the snap to a task composer — the capture
            stays on this device until you send the message.
          </p>
          {!supported ? (
            <p className={cn(SETTINGS_CARD_ROW_DESCRIPTION_CLASS_NAME, "pt-0.5")}>
              {appSnapState
                ? (appSnapState.message ?? "AppSnap is available only in the macOS desktop app.")
                : "AppSnap requires the Synara desktop app on macOS."}
            </p>
          ) : null}
        </div>
      </SettingsCard>

      <SettingsSection title="Capture">
        <SettingsRow
          title="Enable AppSnap"
          description="Run the capture listener in the background while Synara is open."
          status={appSnapStatusText(appSnapState)}
          resetAction={
            settings.enableAppSnap !== defaults.enableAppSnap ? (
              <SettingResetButton
                label="AppSnap"
                onClick={() => void setAppSnapEnabled(defaults.enableAppSnap)}
              />
            ) : null
          }
          control={
            <Switch
              checked={enabled}
              disabled={!supported}
              onCheckedChange={(checked) => void setAppSnapEnabled(Boolean(checked))}
              aria-label="Enable AppSnap"
            />
          }
        />

        <SettingsRow
          title="Shortcut"
          description="Choose exactly two keys: one modifier and one other key. Synara checks its own bindings and asks macOS whether another app already owns the shortcut before saving it."
          control={
            <AppSnapShortcutControl
              key={
                settings.appSnapShortcut.kind === "both-option-keys"
                  ? settings.appSnapShortcut.kind
                  : `${settings.appSnapShortcut.modifier}:${settings.appSnapShortcut.key}`
              }
              shortcut={settings.appSnapShortcut}
              enabled={enabled}
              reserved={enabled && appSnapState?.status === "ready"}
              keybindings={keybindings}
              onSaved={(shortcut, state) => {
                updateSettings({ appSnapShortcut: shortcut });
                setAppSnapState(state);
              }}
            />
          }
        />

        <SettingsRow
          title="Destination"
          description="Snaps join the task you interacted with in the last minute, and consecutive snaps stay together. Otherwise Synara opens a fresh task with the capture attached."
          control={<span className="text-xs font-medium text-muted-foreground">Automatic</span>}
        />

        <SettingsRow
          title="Capture sound"
          description="Play a short shutter cue when a window is captured."
          resetAction={
            settings.appSnapPlaySound !== defaults.appSnapPlaySound ? (
              <SettingResetButton
                label="capture sound"
                onClick={() => updateSettings({ appSnapPlaySound: defaults.appSnapPlaySound })}
              />
            ) : null
          }
          control={
            <div className="flex w-full items-center gap-2 sm:w-auto sm:justify-end">
              <Button size="xs" variant="outline" onClick={() => void playAppSnapCaptureSound()}>
                Preview
              </Button>
              <Switch
                checked={settings.appSnapPlaySound}
                onCheckedChange={(checked) =>
                  updateSettings({ appSnapPlaySound: Boolean(checked) })
                }
                aria-label="Play a sound when an AppSnap is captured"
              />
            </div>
          }
        />
      </SettingsSection>

      {supported ? (
        <SettingsSection title="macOS permissions">
          <SettingsRow
            title="Input Monitoring"
            description="Lets Synara notice the double-Option chord while another app owns the keyboard. Nothing you type is recorded."
            control={<AppSnapPermissionBadge permission={appSnapState.inputMonitoringPermission} />}
          />
          <SettingsRow
            title="Screen Recording"
            description="Lets Synara capture an image of the frontmost window. Only the single window you snap is captured, only at the moment you press the chord."
            control={<AppSnapPermissionBadge permission={appSnapState.screenRecordingPermission} />}
          />
          <SettingsRow
            title="Permission status"
            description="Grant both permissions to Synara under System Settings → Privacy & Security, then recheck here. macOS may require relaunching the app after a change."
            control={
              <Button
                type="button"
                size="xs"
                variant="outline"
                onClick={() => void recheckAppSnapPermissions()}
              >
                Recheck permissions
              </Button>
            }
          />
        </SettingsSection>
      ) : null}
    </div>
  );
}
