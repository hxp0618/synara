import { type ReactNode } from "react";

import { cn } from "~/lib/utils";
import {
  SETTINGS_CONTROL_BORDER_CLASS_NAME,
  SETTINGS_CONTROL_RADIUS_CLASS_NAME,
} from "~/settingsPanelStyles";

export const CONTROL_PLANE_FORM_GRID_CLASS_NAME = "mt-3 grid gap-3 sm:grid-cols-2";
export const CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME = [
  "min-h-9 w-full bg-background px-3 text-[length:var(--app-font-size-ui,12px)] text-foreground outline-none",
  "focus-visible:border-foreground/30 focus-visible:ring-1 focus-visible:ring-ring/60 sm:min-h-8",
  SETTINGS_CONTROL_RADIUS_CLASS_NAME,
  SETTINGS_CONTROL_BORDER_CLASS_NAME,
].join(" ");

export function ControlPlaneFormField(props: { label: string; children: ReactNode }) {
  return (
    <label className="grid gap-1.5">
      <span className="text-[length:var(--app-font-size-ui-sm,11px)] font-medium text-foreground">
        {props.label}
      </span>
      {props.children}
    </label>
  );
}

export function ControlPlaneInlineError({ error }: { error: unknown }) {
  if (!error) return null;
  const message = error instanceof Error ? error.message : "The request failed.";
  return (
    <p
      role="alert"
      className="mt-2 text-[length:var(--app-font-size-ui,12px)] leading-relaxed text-destructive"
    >
      {message}
    </p>
  );
}

export function ControlPlaneStatusPill(props: { value: string; active?: boolean }) {
  const active =
    props.active ?? ["active", "owner", "admin", "live", "selected"].includes(props.value);
  return (
    <span
      className={cn(
        "rounded-full border px-2 py-0.5 text-[length:var(--app-font-size-ui-xs,10px)] font-medium capitalize",
        active
          ? "border-emerald-500/20 bg-emerald-500/8 text-emerald-700 dark:text-emerald-300"
          : "border-border bg-foreground/4 text-muted-foreground",
      )}
    >
      {props.value.replaceAll("_", " ")}
    </span>
  );
}
