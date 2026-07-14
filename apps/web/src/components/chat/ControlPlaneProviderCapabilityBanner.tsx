// FILE: ControlPlaneProviderCapabilityBanner.tsx
// Purpose: Surfaces the active SaaS Provider capability state above the transcript.
// Layer: Chat status presentation
// Exports: ControlPlaneProviderCapabilityBanner

import { memo } from "react";

import type { ControlPlaneDispatchDecision } from "~/lib/controlPlaneProviderCapabilities";
import { CircleAlertIcon, TriangleAlertIcon } from "~/lib/icons";
import { cn } from "~/lib/utils";
import { Alert, AlertDescription, AlertTitle } from "../ui/alert";
import {
  EXPANDED_NOTIFICATION_SURFACE_CLASS_NAME,
  NOTIFICATION_ICON_CLASS_NAME,
} from "../ui/notificationSurface";
import { ChatColumnBannerFrame } from "./ChatColumnBannerFrame";

export const ControlPlaneProviderCapabilityBanner = memo(
  function ControlPlaneProviderCapabilityBanner({
    decision,
  }: {
    decision: ControlPlaneDispatchDecision | null;
  }) {
    if (!decision?.message || (!decision.temporary && decision.allowed)) {
      return null;
    }

    const blocked = !decision.allowed;
    const title = blocked
      ? decision.blockingDecision?.status === "loading"
        ? "Checking SaaS Provider support"
        : decision.blockingDecision?.status === "error"
          ? "Provider capability check unavailable"
          : "Provider unavailable on this SaaS target"
      : "Waiting for a compatible Worker";
    const variant = blocked && !decision.temporary ? "error" : "warning";
    const Icon = variant === "error" ? CircleAlertIcon : TriangleAlertIcon;

    return (
      <ChatColumnBannerFrame>
        <Alert className={cn(EXPANDED_NOTIFICATION_SURFACE_CLASS_NAME)} variant={variant}>
          <Icon className={NOTIFICATION_ICON_CLASS_NAME} />
          <AlertTitle className="font-normal text-[var(--notification-fg)]">{title}</AlertTitle>
          <AlertDescription className="text-[var(--notification-fg)]/72">
            {decision.message}
          </AlertDescription>
        </Alert>
      </ChatColumnBannerFrame>
    );
  },
);
