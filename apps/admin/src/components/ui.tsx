// FILE: ui.tsx
// Purpose: Define the small Admin-host visual contract used by every Platform workflow.
// Layer: Admin design-system primitives

import type {
  ButtonHTMLAttributes,
  InputHTMLAttributes,
  ReactNode,
  SelectHTMLAttributes,
} from "react";

function classes(...values: ReadonlyArray<string | false | null | undefined>): string {
  return values.filter(Boolean).join(" ");
}

export type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  readonly variant?: "primary" | "outline" | "ghost" | "danger";
  readonly size?: "xs" | "sm" | "default";
};

export function Button({
  className,
  variant = "outline",
  size = "default",
  type = "button",
  ...props
}: ButtonProps) {
  return (
    <button
      {...props}
      type={type}
      className={classes("button", `button--${variant}`, `button--${size}`, className)}
    />
  );
}

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return <input {...props} className={classes("control", className)} />;
}

export function Select({ className, ...props }: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select {...props} className={classes("control", "select", className)} />;
}

export function Field(props: { readonly label: string; readonly children: ReactNode }) {
  return (
    <label className="field">
      <span className="field__label">{props.label}</span>
      {props.children}
    </label>
  );
}

export function StatusPill(props: {
  readonly value: string;
  readonly tone?: "active" | "warning" | "neutral" | "danger";
}) {
  return (
    <span className={classes("status", `status--${props.tone ?? "neutral"}`)}>
      {props.value.replaceAll("_", " ")}
    </span>
  );
}

export function InlineError(props: { readonly error: unknown }) {
  if (!props.error) return null;
  return (
    <p className="inline-error" role="alert">
      {props.error instanceof Error ? props.error.message : "The request failed."}
    </p>
  );
}

export function EmptyState(props: { readonly title: string; readonly description?: string }) {
  return (
    <div className="empty-state">
      <strong>{props.title}</strong>
      {props.description ? <p>{props.description}</p> : null}
    </div>
  );
}

export function LoadingState(props: { readonly label?: string }) {
  return (
    <div className="loading-state" role="status">
      <span className="loading-state__dot" aria-hidden />
      {props.label ?? "Loading…"}
    </div>
  );
}
