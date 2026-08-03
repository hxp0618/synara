// FILE: whatsNew/ReleaseNoticeSection.tsx
// Purpose: Render a structured administrator, breaking-change, or urgent
// security notice before ordinary release highlights.

import { Alert, AlertDescription, AlertTitle } from "~/components/ui/alert";
import { Badge } from "~/components/ui/badge";

import type { WhatsNewNotice } from "./logic";

const NOTICE_PRESENTATION = {
  "administrator-action": {
    label: "Administrator action",
    alertVariant: "warning",
    badgeVariant: "warning",
  },
  "breaking-change": {
    label: "Breaking change",
    alertVariant: "error",
    badgeVariant: "error",
  },
  "urgent-security": {
    label: "Urgent security notice",
    alertVariant: "error",
    badgeVariant: "error",
  },
} as const;

export function ReleaseNoticeSection({ notice }: { readonly notice: WhatsNewNotice }) {
  const presentation = NOTICE_PRESENTATION[notice.classification];

  return (
    <Alert variant={presentation.alertVariant} role="note">
      <AlertTitle className="flex flex-wrap items-center gap-2">
        <Badge variant={presentation.badgeVariant}>{presentation.label}</Badge>
        <span>{notice.title}</span>
      </AlertTitle>
      <AlertDescription>
        <p>{notice.summary}</p>
        <dl className="grid gap-x-3 gap-y-1 text-xs sm:grid-cols-[max-content_1fr]">
          <dt className="font-medium text-foreground">Affected</dt>
          <dd>{notice.affectedAudience}</dd>
          <dt className="font-medium text-foreground">Required action</dt>
          <dd>{notice.requiredAction}</dd>
          <dt className="font-medium text-foreground">Effective</dt>
          <dd>
            <time dateTime={notice.effectiveAt}>{notice.effectiveAt}</time>
          </dd>
          <dt className="font-medium text-foreground">First announced</dt>
          <dd>
            <time dateTime={notice.firstPublishedAt}>{notice.firstPublishedAt}</time>
          </dd>
          {notice.classification === "urgent-security" ? (
            <>
              <dt className="font-medium text-foreground">Mitigation</dt>
              <dd>{notice.mitigation}</dd>
              <dt className="font-medium text-foreground">Exception expires</dt>
              <dd>
                <time dateTime={notice.exceptionExpiresAt}>{notice.exceptionExpiresAt}</time>
              </dd>
            </>
          ) : null}
        </dl>
        {notice.migrationGuideUrl !== undefined || notice.supportUrl !== undefined ? (
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs font-medium">
            {notice.migrationGuideUrl !== undefined ? (
              <a
                className="text-foreground underline underline-offset-2"
                href={notice.migrationGuideUrl}
                target="_blank"
                rel="noopener noreferrer"
              >
                Migration guide
              </a>
            ) : null}
            {notice.supportUrl !== undefined ? (
              <a
                className="text-foreground underline underline-offset-2"
                href={notice.supportUrl}
                target="_blank"
                rel="noopener noreferrer"
              >
                Contact support
              </a>
            ) : null}
          </div>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}
