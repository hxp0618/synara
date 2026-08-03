// FILE: PlatformDesktopAccess.tsx
// Purpose: Issue, open, copy, and revoke short-lived device-bound Desktop access.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlaneDesktopDevice,
  type ControlPlaneDesktopEnrollment,
  type ControlPlaneIssuedDesktopEnrollment,
  type ControlPlanePlatformTenantOverview,
  type ControlPlaneSessionState,
} from "@synara/control-plane-client";
import {
  IconBan,
  IconCopy,
  IconDeviceDesktop,
  IconExternalLink,
  IconInfoCircle,
} from "@tabler/icons-react";
import { useEffect, useMemo, useState } from "react";

import {
  Button,
  EmptyState,
  Field,
  InlineError,
  Input,
  Select,
  StatusPill,
} from "../components/ui";
import { buildDesktopEnrollmentLink } from "./desktopEnrollmentLink";
import type { AdminDestination } from "./navigation";
import { platformQueryKeys } from "./platformQueries";

type IssuedInPage = Pick<ControlPlaneIssuedDesktopEnrollment, "handle"> & {
  readonly enrollment: ControlPlaneDesktopEnrollment;
};

type RevokeTarget =
  | { readonly kind: "enrollment"; readonly id: string; readonly version: number }
  | { readonly kind: "device"; readonly id: string; readonly version: number };

function enrollmentTone(status: ControlPlaneDesktopEnrollment["status"]) {
  if (status === "redeemed") return "active" as const;
  if (status === "pending") return "warning" as const;
  if (status === "revoked") return "danger" as const;
  return "neutral" as const;
}

function deviceTone(status: ControlPlaneDesktopDevice["status"]) {
  return status === "active" ? ("active" as const) : ("danger" as const);
}

function openOneTimeLink(issued: IssuedInPage): void {
  window.location.assign(
    buildDesktopEnrollmentLink({
      controlPlaneOrigin: issued.enrollment.controlPlaneOrigin,
      handle: issued.handle,
    }),
  );
}

export function PlatformDesktopAccess(props: {
  readonly overview: ControlPlanePlatformTenantOverview;
  readonly session: ControlPlaneSessionState;
  readonly onNavigate: (destination: AdminDestination) => void;
}) {
  const queryClient = useQueryClient();
  const [tenantId, setTenantId] = useState(props.overview.items[0]?.id ?? "");
  const [subjectUserId, setSubjectUserId] = useState("");
  const [reason, setReason] = useState("");
  const [revocationReason, setRevocationReason] = useState("");
  const [issuedInPage, setIssuedInPage] = useState<IssuedInPage | null>(null);
  const [openAttempted, setOpenAttempted] = useState(false);
  const [localError, setLocalError] = useState<Error | null>(null);
  const access = useQuery({
    queryKey: platformQueryKeys.desktopAccess(tenantId),
    queryFn: () => controlPlaneClient.getPlatformDesktopAccess(tenantId),
    enabled: tenantId.length > 0,
    refetchInterval: 15_000,
  });
  const selectedTenant = props.overview.items.find((tenant) => tenant.id === tenantId) ?? null;
  const safeSubjects = useMemo(
    () => access.data?.subjects.filter((subject) => subject.selfEnrollable) ?? [],
    [access.data?.subjects],
  );
  const selectedSubject =
    access.data?.subjects.find((subject) => subject.userId === subjectUserId) ?? null;

  useEffect(() => {
    const self =
      safeSubjects.find((subject) => subject.userId === props.session.user.userId) ??
      safeSubjects[0];
    if (!self) {
      setSubjectUserId("");
      return;
    }
    if (!safeSubjects.some((subject) => subject.userId === subjectUserId)) {
      setSubjectUserId(self.userId);
    }
  }, [props.session.user.userId, safeSubjects, subjectUserId]);

  useEffect(() => {
    setIssuedInPage(null);
    setOpenAttempted(false);
    setLocalError(null);
  }, [tenantId]);

  const refresh = async () => {
    await queryClient.invalidateQueries({ queryKey: platformQueryKeys.desktopAccess(tenantId) });
  };
  const generateAndOpen = useMutation({
    mutationFn: async () => {
      if (!selectedSubject?.selfEnrollable) {
        throw new Error(
          "The selected subject must complete identity-provider authentication first.",
        );
      }
      const created = await controlPlaneClient.issuePlatformDesktopEnrollment(tenantId, {
        subjectUserId: selectedSubject.userId,
        mode: "connect_existing",
        reason,
      });
      const opened = await controlPlaneClient.markPlatformDesktopEnrollmentOpened(
        created.enrollment.id,
        created.enrollment.version,
      );
      return { handle: created.handle, enrollment: opened } satisfies IssuedInPage;
    },
    onSuccess: async (issued) => {
      setIssuedInPage(issued);
      setOpenAttempted(true);
      setLocalError(null);
      await refresh();
      try {
        openOneTimeLink(issued);
      } catch {
        setLocalError(
          new Error(
            "Synara Desktop could not be opened. Install it, then retry while this link remains valid.",
          ),
        );
      }
    },
  });
  const reopen = useMutation({
    mutationFn: async () => {
      if (!issuedInPage) throw new Error("Generate a new one-time Desktop link.");
      const opened = await controlPlaneClient.markPlatformDesktopEnrollmentOpened(
        issuedInPage.enrollment.id,
        issuedInPage.enrollment.version,
      );
      return { ...issuedInPage, enrollment: opened } satisfies IssuedInPage;
    },
    onSuccess: async (issued) => {
      setIssuedInPage(issued);
      setOpenAttempted(true);
      setLocalError(null);
      await refresh();
      try {
        openOneTimeLink(issued);
      } catch {
        setLocalError(
          new Error(
            "Synara Desktop could not be opened. Install it, then retry while this link remains valid.",
          ),
        );
      }
    },
  });
  const copy = useMutation({
    mutationFn: async () => {
      if (!issuedInPage) throw new Error("Generate a new one-time Desktop link.");
      const opened = await controlPlaneClient.markPlatformDesktopEnrollmentOpened(
        issuedInPage.enrollment.id,
        issuedInPage.enrollment.version,
      );
      const next = { ...issuedInPage, enrollment: opened } satisfies IssuedInPage;
      await navigator.clipboard.writeText(
        buildDesktopEnrollmentLink({
          controlPlaneOrigin: next.enrollment.controlPlaneOrigin,
          handle: next.handle,
        }),
      );
      return next;
    },
    onSuccess: async (issued) => {
      setIssuedInPage(issued);
      setLocalError(null);
      await refresh();
    },
  });
  const revoke = useMutation({
    mutationFn: async (target: RevokeTarget) => {
      const input = { expectedVersion: target.version, reason: revocationReason };
      if (target.kind === "enrollment") {
        return controlPlaneClient.revokePlatformDesktopEnrollment(target.id, input);
      }
      return controlPlaneClient.revokePlatformDesktopDevice(target.id, input);
    },
    onSuccess: async (result) => {
      if (issuedInPage?.enrollment.id === result.id) setIssuedInPage(null);
      setRevocationReason("");
      await refresh();
    },
  });
  const pending =
    access.data?.enrollments.filter((enrollment) => enrollment.status === "pending") ?? [];
  const devices = access.data?.devices ?? [];
  const expiryMinutes = access.data
    ? Math.max(1, Math.round(access.data.enrollmentTtlSeconds / 60))
    : null;

  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <p className="page-heading__back">
            <button onClick={() => props.onNavigate("tenants")}>Tenant operations</button> / Desktop
            access
          </p>
          <h1>Desktop access</h1>
          <p>
            Connect one named internal user identity to one device without transferring Platform
            authority.
          </p>
          <p className="authority-note">
            <IconInfoCircle aria-hidden size={16} /> Unverified cross-user enrollment requires
            subject IdP authentication; Platform authority cannot mint their Desktop session or
            return a link.
          </p>
        </div>
      </header>

      <section className="form-panel" aria-labelledby="desktop-generate-heading">
        <header className="section-heading">
          <div>
            <h2 id="desktop-generate-heading">Generate and open Synara Desktop</h2>
            <p>The raw one-time link stays only in this page's memory and disappears on reload.</p>
          </div>
          <IconDeviceDesktop aria-hidden size={22} />
        </header>
        {props.overview.items.length === 0 ? (
          <EmptyState title="No internal Tenants" />
        ) : (
          <form
            className="form-grid"
            onSubmit={(event) => {
              event.preventDefault();
              generateAndOpen.mutate();
            }}
          >
            <Field label="Internal Tenant">
              <Select onChange={(event) => setTenantId(event.target.value)} value={tenantId}>
                {props.overview.items.map((tenant) => (
                  <option key={tenant.id} value={tenant.id}>
                    {tenant.name} · {tenant.status}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Subject user">
              <Select
                disabled={!access.data || access.data.subjects.length === 0}
                onChange={(event) => setSubjectUserId(event.target.value)}
                value={subjectUserId}
              >
                <option value="">Select a subject</option>
                {access.data?.subjects.map((subject) => (
                  <option
                    disabled={!subject.selfEnrollable}
                    key={subject.userId}
                    value={subject.userId}
                  >
                    {subject.displayName || subject.email} · {subject.role}
                    {subject.selfEnrollable ? "" : " · IdP required"}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Auditable reason">
              <Input
                maxLength={1000}
                minLength={10}
                onChange={(event) => setReason(event.target.value)}
                required
                value={reason}
              />
            </Field>
            <div className="desktop-context form-grid__wide" aria-label="Enrollment context">
              <div>
                <span>Subject</span>
                <strong>
                  {selectedSubject
                    ? `${selectedSubject.displayName || selectedSubject.email} · ${selectedSubject.role}`
                    : "No safe subject"}
                </strong>
              </div>
              <div>
                <span>Tenant</span>
                <strong>{selectedTenant?.name ?? "None"}</strong>
              </div>
              <div>
                <span>Organization</span>
                <strong>Tenant default</strong>
              </div>
              <div>
                <span>Expiry</span>
                <strong>{expiryMinutes ? `${expiryMinutes} minutes` : "Loading…"}</strong>
              </div>
              <div>
                <span>Control Plane</span>
                <strong>{access.data?.controlPlaneOrigin || "Unavailable"}</strong>
              </div>
            </div>
            {!access.isPending && safeSubjects.length === 0 ? (
              <div className="boundary-callout form-grid__wide">
                No subject is eligible for automatic self-enrollment. The selected internal user
                must authenticate through the configured IdP; Platform authority cannot mint their
                Desktop session.
              </div>
            ) : null}
            <div className="form-actions form-grid__wide">
              <Button
                disabled={
                  generateAndOpen.isPending ||
                  !selectedSubject?.selfEnrollable ||
                  reason.trim().length < 10 ||
                  !access.data?.controlPlaneOrigin
                }
                type="submit"
                variant="primary"
              >
                <IconExternalLink aria-hidden size={16} />
                {generateAndOpen.isPending ? "Generating…" : "Generate and open Synara Desktop"}
              </Button>
            </div>
            <div className="form-grid__wide">
              <InlineError error={access.error ?? generateAndOpen.error} />
            </div>
          </form>
        )}
      </section>

      {issuedInPage ? (
        <section
          className="form-panel desktop-link-fallback"
          aria-labelledby="desktop-link-heading"
        >
          <header className="section-heading">
            <div>
              <h2 id="desktop-link-heading">One-time link ready</h2>
              <p>
                It expires {new Date(issuedInPage.enrollment.expiresAt).toLocaleString()}. Do not
                paste it into tickets or chat.
              </p>
            </div>
            <StatusPill value={issuedInPage.enrollment.status} tone="warning" />
          </header>
          {openAttempted ? (
            <div className="boundary-callout">
              If Desktop did not open, install Synara and retry before expiry. A consumed or expired
              link cannot be reissued.
            </div>
          ) : null}
          <div className="form-actions">
            <Button
              disabled={reopen.isPending || issuedInPage.enrollment.status !== "pending"}
              onClick={() => reopen.mutate()}
              variant="primary"
            >
              <IconExternalLink aria-hidden size={16} />
              Open Synara Desktop
            </Button>
            <Button
              disabled={copy.isPending || issuedInPage.enrollment.status !== "pending"}
              onClick={() => copy.mutate()}
            >
              <IconCopy aria-hidden size={16} />
              Copy one-time Desktop link
            </Button>
          </div>
          <InlineError error={localError ?? reopen.error ?? copy.error} />
        </section>
      ) : null}

      <section className="support-list" aria-labelledby="desktop-records-heading">
        <header className="section-heading">
          <div>
            <h2 id="desktop-records-heading">Pending Enrollments and connected devices</h2>
            <p>Revocation takes effect at the Control Plane; local projects remain available.</p>
          </div>
        </header>
        <Field label="Revocation reason">
          <Input
            maxLength={1000}
            minLength={10}
            onChange={(event) => setRevocationReason(event.target.value)}
            placeholder="Required before revocation"
            value={revocationReason}
          />
        </Field>
        {access.isPending ? (
          <p className="loading-state">Loading Desktop access…</p>
        ) : pending.length === 0 && devices.length === 0 ? (
          <EmptyState
            title="No Desktop access records"
            description="Generate a short-lived Enrollment when a safe subject is available."
          />
        ) : (
          <div className="request-rows desktop-access-rows">
            {pending.map((enrollment) => (
              <article className="request-row" key={enrollment.id}>
                <div>
                  <strong>{enrollment.subjectDisplayName || enrollment.subjectEmail}</strong>
                  <p>{enrollment.reason}</p>
                  <small>
                    Pending Enrollment · expires {new Date(enrollment.expiresAt).toLocaleString()}
                  </small>
                </div>
                <div className="request-row__actions">
                  <StatusPill value={enrollment.status} tone={enrollmentTone(enrollment.status)} />
                  <Button
                    disabled={revoke.isPending || revocationReason.trim().length < 10}
                    onClick={() =>
                      revoke.mutate({
                        kind: "enrollment",
                        id: enrollment.id,
                        version: enrollment.version,
                      })
                    }
                    size="sm"
                    variant="danger"
                  >
                    <IconBan aria-hidden size={15} />
                    Revoke
                  </Button>
                </div>
              </article>
            ))}
            {devices.map((device) => (
              <article className="request-row" key={device.id}>
                <div>
                  <strong>{device.deviceLabel}</strong>
                  <p>
                    {device.userDisplayName || device.userEmail} · {device.platform} · Synara{" "}
                    {device.appVersion}
                  </p>
                  <small>Last seen {new Date(device.lastSeenAt).toLocaleString()}</small>
                </div>
                <div className="request-row__actions">
                  <StatusPill value={device.status} tone={deviceTone(device.status)} />
                  {device.status === "active" ? (
                    <Button
                      disabled={revoke.isPending || revocationReason.trim().length < 10}
                      onClick={() =>
                        revoke.mutate({ kind: "device", id: device.id, version: device.version })
                      }
                      size="sm"
                      variant="danger"
                    >
                      <IconBan aria-hidden size={15} />
                      Revoke
                    </Button>
                  ) : null}
                </div>
              </article>
            ))}
          </div>
        )}
        <InlineError error={access.error ?? revoke.error} />
      </section>
    </div>
  );
}
