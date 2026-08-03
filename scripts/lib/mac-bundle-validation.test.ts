import { assert, describe, it } from "@effect/vitest";

import { parseCodesignTeamId, validateMacBinaryArchitectures } from "./mac-bundle-validation.ts";

describe("mac bundle validation", () => {
  it("accepts native and universal components in an arm64 artifact", () => {
    assert.doesNotThrow(() =>
      validateMacBinaryArchitectures(
        [
          { path: "Contents/MacOS/Synara", architectures: ["arm64"] },
          { path: "Contents/Frameworks/helper", architectures: ["x86_64", "arm64"] },
        ],
        "arm64",
      ),
    );
  });

  it("rejects Intel-only components in an arm64 artifact", () => {
    assert.throws(
      () =>
        validateMacBinaryArchitectures(
          [{ path: "Contents/Helpers/legacy", architectures: ["x86_64"] }],
          "arm64",
        ),
      /arm64 artifact contains incompatible Mach-O components.*legacy.*x86_64/u,
    );
  });

  it("maps the x64 artifact target to the Mach-O x86_64 architecture", () => {
    assert.doesNotThrow(() =>
      validateMacBinaryArchitectures(
        [
          { path: "Contents/MacOS/Synara", architectures: ["x86_64"] },
          { path: "Contents/Frameworks/helper", architectures: ["arm64", "x86_64"] },
        ],
        "x64",
      ),
    );
    assert.throws(
      () =>
        validateMacBinaryArchitectures(
          [{ path: "Contents/MacOS/Synara", architectures: ["arm64"] }],
          "x64",
        ),
      /x64 artifact contains incompatible Mach-O components.*Synara.*arm64/u,
    );
  });

  it("accepts paired architecture-qualified native dependency prebuilds", () => {
    const pairedPrebuilds = [
      {
        path: "Contents/Resources/app.asar.unpacked/node_modules/node-pty/prebuilds/darwin-arm64/pty.node",
        architectures: ["arm64"],
      },
      {
        path: "Contents/Resources/app.asar.unpacked/node_modules/node-pty/prebuilds/darwin-x64/pty.node",
        architectures: ["x86_64"],
      },
    ];
    assert.doesNotThrow(() => validateMacBinaryArchitectures(pairedPrebuilds, "x64"));
    assert.doesNotThrow(() => validateMacBinaryArchitectures(pairedPrebuilds, "universal"));
  });

  it("rejects an unpaired foreign native dependency prebuild", () => {
    assert.throws(
      () =>
        validateMacBinaryArchitectures(
          [
            {
              path: "Contents/Resources/app.asar.unpacked/node_modules/node-pty/prebuilds/darwin-arm64/pty.node",
              architectures: ["arm64"],
            },
          ],
          "x64",
        ),
      /x64 artifact contains incompatible Mach-O components.*darwin-arm64/u,
    );
  });

  it("requires both slices for a universal artifact", () => {
    assert.throws(
      () =>
        validateMacBinaryArchitectures(
          [{ path: "Contents/MacOS/Synara", architectures: ["arm64"] }],
          "universal",
        ),
      /universal artifact contains incompatible/u,
    );
  });

  it("parses only concrete code-signing team identities", () => {
    assert.equal(
      parseCodesignTeamId("Authority=Apple Development\nTeamIdentifier=TEAM123\n"),
      "TEAM123",
    );
    assert.equal(parseCodesignTeamId("TeamIdentifier=not set\n"), null);
    assert.equal(parseCodesignTeamId("Signature=adhoc\n"), null);
  });
});
