// FILE: 090_ExternalMcpSecuritySignals.ts
// Purpose: Persists bounded, plaintext-free prompt-injection provenance on external MCP audit rows.

import * as Effect from "effect/Effect";
import * as SqlClient from "effect/unstable/sql/SqlClient";

import { columnExists } from "./schemaHelpers.ts";

export default Effect.gen(function* () {
  const sql = yield* SqlClient.SqlClient;
  const columns = [
    ["content_source", "TEXT"],
    ["content_trust", "TEXT"],
    ["content_sha256", "TEXT"],
    ["content_risk", "TEXT"],
    ["content_indicator_ids_json", "TEXT NOT NULL DEFAULT '[]'"],
    ["security_alert_kind", "TEXT"],
  ] as const;
  for (const [name, declaration] of columns) {
    if (!(yield* columnExists(sql, "external_mcp_audit_log", name))) {
      yield* sql.unsafe(`ALTER TABLE external_mcp_audit_log ADD COLUMN ${name} ${declaration}`);
    }
  }
  yield* sql`
    CREATE INDEX IF NOT EXISTS idx_external_mcp_audit_security_alert
    ON external_mcp_audit_log (integration_id, security_alert_kind, created_at)
    WHERE security_alert_kind IS NOT NULL
  `;
});
