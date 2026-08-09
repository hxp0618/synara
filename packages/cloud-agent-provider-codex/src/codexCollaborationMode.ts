import { PROVIDER_CONTENT_TRUST_POLICY } from "./providerContentTrustPolicy";
export type CodexCollaborationMode = "default" | "plan";
export const CODEX_DEFAULT_MODE_DEVELOPER_INSTRUCTIONS = `<collaboration_mode># Collaboration Mode: Default\nExecute the current request while preserving Host authority and approval boundaries.\n</collaboration_mode>\n\n${PROVIDER_CONTENT_TRUST_POLICY}`;
export const CODEX_PLAN_MODE_DEVELOPER_INSTRUCTIONS = `<collaboration_mode># Plan Mode (Conversational)\nPerform read-only exploration and produce a decision-complete plan; do not mutate repository state.\n</collaboration_mode>\n\n${PROVIDER_CONTENT_TRUST_POLICY}`;
export function codexDeveloperInstructionsForMode(mode: CodexCollaborationMode): string {
  return mode === "plan"
    ? CODEX_PLAN_MODE_DEVELOPER_INSTRUCTIONS
    : CODEX_DEFAULT_MODE_DEVELOPER_INSTRUCTIONS;
}
