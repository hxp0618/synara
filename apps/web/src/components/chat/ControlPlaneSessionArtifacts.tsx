// FILE: ControlPlaneSessionArtifacts.tsx
// Purpose: Shows durable SaaS Session Artifacts with retry-safe download actions.
// Layer: Chat status presentation
// Exports: ControlPlaneSessionArtifacts

import { formatBytes } from "@synara/shared/formatBytes";
import { memo, useMemo } from "react";

import type { ControlPlaneArtifact } from "~/lib/controlPlaneClient";
import {
  controlPlaneArtifactDisplayName,
  controlPlaneArtifactKindLabel,
  userDownloadableControlPlaneArtifacts,
} from "~/lib/controlPlaneArtifacts";
import { DownloadIcon, FileIcon, PaperclipIcon, RefreshCwIcon } from "~/lib/icons";
import { cn } from "~/lib/utils";
import { DisclosureRegion } from "../ui/DisclosureRegion";
import { ChatColumnBannerFrame } from "./ChatColumnBannerFrame";

export const ControlPlaneSessionArtifacts = memo(function ControlPlaneSessionArtifacts(props: {
  artifacts?: ReadonlyArray<ControlPlaneArtifact> | undefined;
  error?: Error | null | undefined;
  downloadingArtifactId?: string | null | undefined;
  onDownload: (artifact: ControlPlaneArtifact) => void;
  onRetry: () => void;
}) {
  const artifacts = useMemo(
    () => userDownloadableControlPlaneArtifacts(props.artifacts ?? []),
    [props.artifacts],
  );
  const open = artifacts.length > 0 || (props.error !== null && props.error !== undefined);
  const downloadPending =
    props.downloadingArtifactId !== null && props.downloadingArtifactId !== undefined;

  return (
    <DisclosureRegion open={open}>
      <ChatColumnBannerFrame>
        <section
          aria-label="Session artifacts"
          data-testid="control-plane-session-artifacts"
          className="overflow-hidden rounded-2xl border border-border/55 bg-background/72 shadow-sm backdrop-blur-xl"
        >
          <div className="flex min-w-0 items-center gap-2.5 border-b border-border/45 px-3.5 py-2.5">
            <span className="flex size-7 shrink-0 items-center justify-center rounded-lg border border-border/45 bg-background/80 text-muted-foreground/62">
              <PaperclipIcon className="size-3.5" />
            </span>
            <div className="min-w-0 flex-1">
              <h3 className="text-[12px] font-medium text-foreground/86">
                {props.error ? "Artifacts unavailable" : "Artifacts"}
              </h3>
              <p className="truncate text-[10px] text-muted-foreground/52">
                {props.error
                  ? "Synara could not refresh this Session's ready files."
                  : `${artifacts.length} ready ${artifacts.length === 1 ? "file" : "files"}`}
              </p>
            </div>
            {props.error ? (
              <button
                type="button"
                className="inline-flex shrink-0 items-center gap-1.5 rounded-lg border border-border/55 px-2.5 py-1.5 text-[10px] font-medium text-muted-foreground/72 transition-colors hover:bg-[var(--color-background-button-secondary-hover)] hover:text-foreground"
                onClick={props.onRetry}
              >
                <RefreshCwIcon className="size-3" />
                Retry
              </button>
            ) : null}
          </div>

          {artifacts.length > 0 ? (
            <div className="max-h-48 divide-y divide-border/35 overflow-y-auto">
              {artifacts.map((artifact) => {
                const displayName = controlPlaneArtifactDisplayName(artifact);
                const downloading = props.downloadingArtifactId === artifact.id;
                const metadata = [
                  controlPlaneArtifactKindLabel(artifact.kind),
                  artifact.sizeBytes === null ? null : formatBytes(artifact.sizeBytes),
                ]
                  .filter((value): value is string => value !== null)
                  .join(" · ");
                return (
                  <button
                    key={artifact.id}
                    type="button"
                    aria-label={`Download ${displayName}`}
                    data-artifact-id={artifact.id}
                    className="group/artifact flex w-full min-w-0 items-center gap-2.5 px-3.5 py-2.5 text-left transition-colors hover:bg-[var(--color-background-button-secondary-hover)] disabled:cursor-wait disabled:opacity-65"
                    disabled={downloadPending}
                    onClick={() => props.onDownload(artifact)}
                  >
                    <span className="flex size-7 shrink-0 items-center justify-center rounded-lg bg-muted/35 text-muted-foreground/55 transition-colors group-hover/artifact:text-foreground/72">
                      <FileIcon className="size-3.5" />
                    </span>
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-[12px] font-medium text-foreground/82">
                        {displayName}
                      </span>
                      <span className="block truncate text-[10px] text-muted-foreground/50">
                        {metadata}
                      </span>
                    </span>
                    {downloading ? (
                      <RefreshCwIcon className="size-3.5 shrink-0 animate-spin text-muted-foreground/62 motion-reduce:animate-none" />
                    ) : (
                      <DownloadIcon
                        className={cn(
                          "size-3.5 shrink-0 text-muted-foreground/42 transition-colors",
                          "group-hover/artifact:text-foreground/78",
                        )}
                      />
                    )}
                  </button>
                );
              })}
            </div>
          ) : null}
        </section>
      </ChatColumnBannerFrame>
    </DisclosureRegion>
  );
});
