import { describe, expect, it } from "vitest";
import { parse } from "yaml";

import { buildStage7PublicOpenAPI } from "./stage7-public-openapi";

describe("buildStage7PublicOpenAPI", () => {
  it("publishes only codegen-ready operations and removes the internal route inventory", () => {
    const result = buildStage7PublicOpenAPI(`
openapi: 3.1.0
info: { title: Test, version: 1.0.0 }
paths:
  /ready:
    get:
      operationId: ready
      x-synara-contract-status: codegen-ready
      responses: { "200": { description: ok } }
    post:
      operationId: notReady
      x-synara-contract-status: route-only
      responses: { default: { description: error } }
x-synara-route-surfaces:
  - { method: GET, path: /ready, x-synara-exposure: public-beta }
components: {}
`);
    const document = parse(result.document);

    expect(result.operationIds).toEqual(["ready"]);
    expect(document.paths["/ready"].get.operationId).toBe("ready");
    expect(document.paths["/ready"].post).toBeUndefined();
    expect(document["x-synara-route-surfaces"]).toBeUndefined();
  });

  it("fails rather than publishing an empty or malformed contract", () => {
    expect(() =>
      buildStage7PublicOpenAPI(`openapi: 3.1.0\ninfo: { title: Test }\npaths: {}`),
    ).toThrow(/no codegen-ready operations/);
    expect(() =>
      buildStage7PublicOpenAPI(`
openapi: 3.1.0
info: { title: Test }
paths:
  /broken:
    get:
      x-synara-contract-status: codegen-ready
`),
    ).toThrow(/without an operationId/);
  });
});
