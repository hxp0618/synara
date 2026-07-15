// FILE: ControlPlaneSessionStreamBanner.tsx
// Purpose: Surfaces authoritative SaaS Session Event reconnect failures above the transcript.
// Layer: Chat status presentation
// Exports: ControlPlaneSessionStreamBanner

import { memo } from "react";

import type { ControlPlaneStreamStatus } from "~/lib/controlPlaneProjection";
import { CircleAlertIcon, RefreshCwIcon } from "~/lib/icons";
import { cn } from "~/lib/utils";
import { Alert, AlertDescription, AlertTitle } from "../ui/alert";
import { DisclosureRegion } from "../ui/DisclosureRegion";
import {
  EXPANDED_NOTIFICATION_SURFACE_CLASS_NAME,
  NOTIFICATION_ICON_CLASS_NAME,
} from "../ui/notificationSurface";
import { ChatColumnBannerFrame } from "./ChatColumnBannerFrame";

export const ControlPlaneSessionStreamBanner = memo(
  function ControlPlaneSessionStreamBanner({
    status,
  }: {
    status: ControlPlaneStreamStatus | null;
  }) {
    const unavailable = status === "error";
    const open = unavailable || status === "reconnecting";
    const Icon = unavailable ? CircleAlertIcon : RefreshCwIcon;

    return (
      <DisclosureRegion open={open}>
        <ChatColumnBannerFrame>
          <Alert
            aria-live="polite"
            className={cn(EXPANDED_NOTIFICATION_SURFACE_CLASS_NAME)}
            variant={unavailable ? "error" : "warning"}
          >
            <Icon
              className={cn(
                NOTIFICATION_ICON_CLASS_NAME,
                !unavailable && "motion-safe:animate-spin",
              )}
            />
            <AlertTitle className="font-normal text-[var(--notification-fg)]">
              {unavailable
                ? "Session Event stream unavailable"
                : "Reconnecting to Session Events"}
            </AlertTitle>
            <AlertDescription className="text-[var(--notification-fg)]/72">
              {unavailable
                ? "Live updates are unavailable. Reopen this Session to retry from the last persisted event sequence."
                : "Live updates are paused. The Session remains authoritative in the Control Plane and will resume from the last persisted event sequence."}
            </AlertDescription>
          </Alert>
        </ChatColumnBannerFrame>
      </DisclosureRegion>
    );
  },
);
