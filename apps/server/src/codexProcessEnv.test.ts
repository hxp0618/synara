// FILE: codexProcessEnv.test.ts
// Purpose: Covers Codex account home-overlay auth isolation guarantees.
// Layer: Server utility tests.
// Exports: Vitest coverage for apps/server/src/codexProcessEnv.ts.
import {
  copyFileSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readlinkSync,
  rmSync,
  symlinkSync,
  chmodSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import OS from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import { pathToFileURL } from "node:url";
import { afterEach, assert, describe, it } from "@effect/vitest";
import { expect, vi } from "vitest";

import {
  buildCodexProcessLaunchContext,
  buildCodexProcessEnv,
  isCodexSharedContinuationStatePrepared,
  linkOrCopyCodexOverlayEntry,
  prioritizeCodexOverlayEntries,
  readCodexAuthTrackingFingerprint,
  readEffectiveCodexAuthCredentialsStoreMode,
  resolveCodexAuthTracking,
} from "./codexProcessEnv.ts";
import { resolveActiveCodexHomeWritePath } from "./codexHomePaths.ts";

describe("readEffectiveCodexAuthCredentialsStoreMode", () => {
  it("uses the selected profile override", () => {
    assert.strictEqual(
      readEffectiveCodexAuthCredentialsStoreMode(
        'profile = "work"\ncli_auth_credentials_store = "file"\n\n[profiles.work]\ncli_auth_credentials_store = "auto"\n',
      ),
      "auto",
    );
    assert.strictEqual(
      readEffectiveCodexAuthCredentialsStoreMode(
        '"profile" = "work"\n"cli_auth_credentials_store" = "file"\nprofiles."work"."cli_auth_credentials_store" = "keyring"\n',
      ),
      "keyring",
    );
  });

  it("defaults to file and ignores unrelated table keys", () => {
    assert.strictEqual(readEffectiveCodexAuthCredentialsStoreMode('model = "gpt-5.4"\n'), "file");
    assert.strictEqual(
      readEffectiveCodexAuthCredentialsStoreMode(
        '[model_providers.local]\ncli_auth_credentials_store = "keyring"\n',
      ),
      "file",
    );
  });
});

describe("buildCodexProcessEnv account overlays", () => {
  const tempRoots: string[] = [];

  function makeTempRoot(): string {
    const root = mkdtempSync(path.join(OS.tmpdir(), "codex-process-env-test-"));
    tempRoots.push(root);
    return root;
  }

  afterEach(() => {
    while (tempRoots.length > 0) {
      const root = tempRoots.pop();
      if (root) {
        rmSync(root, { recursive: true, force: true });
      }
    }
  });

  function makeAccountFixture(input: { readonly shadowAuth: "real" | "symlink" | "missing" }) {
    const root = makeTempRoot();
    const homePath = path.join(root, "codex-home");
    const shadowHomePath = path.join(root, "codex-shadow-work");
    mkdirSync(homePath, { recursive: true });
    mkdirSync(shadowHomePath, { recursive: true });
    writeFileSync(path.join(homePath, "auth.json"), '{"account":"default"}', "utf8");
    writeFileSync(path.join(homePath, "config.toml"), "", "utf8");
    if (input.shadowAuth === "real") {
      writeFileSync(path.join(shadowHomePath, "auth.json"), '{"account":"work"}', "utf8");
    }
    if (input.shadowAuth === "symlink") {
      symlinkSync(path.join(homePath, "auth.json"), path.join(shadowHomePath, "auth.json"));
    }
    const env: NodeJS.ProcessEnv = {
      HOME: root,
      SYNARA_HOME: path.join(root, "synara-runtime"),
    };
    return { env, homePath, shadowHomePath };
  }

  function aliasHomeThroughParent(homePath: string): string {
    const aliasRoot = makeTempRoot();
    const parentAlias = path.join(aliasRoot, "parent-alias");
    symlinkSync(path.dirname(homePath), parentAlias, "dir");
    return path.join(parentAlias, path.basename(homePath));
  }

  function makeAuthCopyOnlyLinker() {
    return {
      symlink: vi.fn((sourcePath: string, targetPath: string, type?: string | null) => {
        if (path.basename(targetPath) === "auth.json") {
          throw new Error("auth symlinks unavailable");
        }
        return symlinkSync(sourcePath, targetPath, type as "dir" | "file");
      }) as typeof symlinkSync,
      copyFile: copyFileSync,
    };
  }

  it("links account-private auth from the shadow home instead of the shared home", () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });

    const env = buildCodexProcessEnv({
      env: fixture.env,
      homePath: fixture.homePath,
      shadowHomePath: fixture.shadowHomePath,
      accountId: "work",
      platform: "win32",
    });

    const overlayHomePath = env.CODEX_HOME;
    assert.ok(overlayHomePath);
    assert.notStrictEqual(path.resolve(overlayHomePath), path.resolve(fixture.homePath));
    const overlayAuthPath = path.join(overlayHomePath, "auth.json");
    assert.ok(lstatSync(overlayAuthPath).isSymbolicLink());
    assert.strictEqual(
      path.resolve(readlinkSync(overlayAuthPath)),
      path.resolve(path.join(fixture.shadowHomePath, "auth.json")),
    );
  });

  it("shares fresh Codex continuation state across account overlays while keeping private state separate", () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });
    const personalShadowHomePath = path.join(
      path.dirname(fixture.shadowHomePath),
      "codex-shadow-personal",
    );
    mkdirSync(personalShadowHomePath, { recursive: true });
    writeFileSync(path.join(personalShadowHomePath, "auth.json"), '{"account":"personal"}', "utf8");
    writeFileSync(
      path.join(personalShadowHomePath, "models_cache.json"),
      '{"model":"personal"}',
      "utf8",
    );
    writeFileSync(
      path.join(fixture.shadowHomePath, "models_cache.json"),
      '{"model":"work"}',
      "utf8",
    );

    const personalEnv = buildCodexProcessEnv({
      env: fixture.env,
      homePath: fixture.homePath,
      shadowHomePath: personalShadowHomePath,
      accountId: "personal",
      platform: "win32",
    });
    assert.ok(personalEnv.CODEX_HOME);
    assert.ok(
      isCodexSharedContinuationStatePrepared({
        env: fixture.env,
        homePath: fixture.homePath,
        shadowHomePath: personalShadowHomePath,
        accountId: "personal",
      }),
    );
    const personalSessionsPath = path.join(personalEnv.CODEX_HOME, "sessions");
    const personalStateDbPath = path.join(personalEnv.CODEX_HOME, "state_5.sqlite");
    assert.ok(lstatSync(personalSessionsPath).isSymbolicLink());
    assert.ok(lstatSync(personalStateDbPath).isSymbolicLink());
    writeFileSync(
      path.join(personalSessionsPath, "thread-personal.jsonl"),
      "personal-thread",
      "utf8",
    );
    writeFileSync(personalStateDbPath, "shared-state", "utf8");

    const workEnv = buildCodexProcessEnv({
      env: fixture.env,
      homePath: fixture.homePath,
      shadowHomePath: fixture.shadowHomePath,
      accountId: "work",
      platform: "win32",
    });
    assert.ok(workEnv.CODEX_HOME);
    assert.notStrictEqual(workEnv.CODEX_HOME, personalEnv.CODEX_HOME);
    assert.strictEqual(
      readFileSync(path.join(workEnv.CODEX_HOME, "sessions", "thread-personal.jsonl"), "utf8"),
      "personal-thread",
    );
    assert.strictEqual(
      readFileSync(path.join(workEnv.CODEX_HOME, "state_5.sqlite"), "utf8"),
      "shared-state",
    );
    assert.strictEqual(
      path.resolve(readlinkSync(path.join(personalEnv.CODEX_HOME, "models_cache.json"))),
      path.resolve(path.join(personalShadowHomePath, "models_cache.json")),
    );
    assert.strictEqual(
      path.resolve(readlinkSync(path.join(workEnv.CODEX_HOME, "models_cache.json"))),
      path.resolve(path.join(fixture.shadowHomePath, "models_cache.json")),
    );
  });

  it("preserves non-empty legacy-only continuation state and requires an explicit migration", () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });
    const legacyOverlayHomePath = resolveActiveCodexHomeWritePath({
      env: fixture.env,
      homePath: fixture.homePath,
      shadowHomePath: fixture.shadowHomePath,
      accountId: "work",
    });
    mkdirSync(path.join(legacyOverlayHomePath, "sessions"), { recursive: true });
    writeFileSync(
      path.join(legacyOverlayHomePath, "sessions", "legacy.jsonl"),
      "legacy-thread",
      "utf8",
    );
    writeFileSync(path.join(legacyOverlayHomePath, "state_4.sqlite"), "legacy-state", "utf8");

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: fixture.env,
          homePath: fixture.homePath,
          shadowHomePath: fixture.shadowHomePath,
          accountId: "work",
          platform: "win32",
        }),
      /refusing to migrate legacy state automatically/,
    );
    assert.strictEqual(
      readFileSync(path.join(legacyOverlayHomePath, "sessions", "legacy.jsonl"), "utf8"),
      "legacy-thread",
    );
    assert.strictEqual(
      readFileSync(path.join(legacyOverlayHomePath, "state_4.sqlite"), "utf8"),
      "legacy-state",
    );
    assert.throws(() => lstatSync(path.join(fixture.homePath, "sessions")));
    assert.throws(() => lstatSync(path.join(fixture.homePath, "state_4.sqlite")));
  });

  it("fails closed and preserves both copies when legacy continuation state conflicts", () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });
    const legacyOverlayHomePath = resolveActiveCodexHomeWritePath({
      env: fixture.env,
      homePath: fixture.homePath,
      shadowHomePath: fixture.shadowHomePath,
      accountId: "work",
    });
    mkdirSync(path.join(fixture.homePath, "sessions"), { recursive: true });
    mkdirSync(path.join(legacyOverlayHomePath, "sessions"), { recursive: true });
    writeFileSync(path.join(fixture.homePath, "sessions", "source.jsonl"), "source", "utf8");
    writeFileSync(path.join(legacyOverlayHomePath, "sessions", "overlay.jsonl"), "overlay", "utf8");

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: fixture.env,
          homePath: fixture.homePath,
          shadowHomePath: fixture.shadowHomePath,
          accountId: "work",
          platform: "win32",
        }),
      /refusing to replace either copy/,
    );
    assert.strictEqual(
      readFileSync(path.join(fixture.homePath, "sessions", "source.jsonl"), "utf8"),
      "source",
    );
    assert.strictEqual(
      readFileSync(path.join(legacyOverlayHomePath, "sessions", "overlay.jsonl"), "utf8"),
      "overlay",
    );
    assert.strictEqual(
      isCodexSharedContinuationStatePrepared({ env: fixture.env, homePath: fixture.homePath }),
      false,
    );
  });

  it("fails closed when account continuation state cannot be symlinked on the host platform", () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });
    const copyOnlyLinker = {
      symlink: vi.fn(() => {
        throw new Error("symlinks unavailable");
      }),
      copyFile: copyFileSync,
    };

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: fixture.env,
          homePath: fixture.homePath,
          shadowHomePath: fixture.shadowHomePath,
          accountId: "work",
          platform: "win32",
          overlayEntryLinker: copyOnlyLinker,
        }),
      /symlinks unavailable/,
    );
    assert.strictEqual(
      isCodexSharedContinuationStatePrepared({ env: fixture.env, homePath: fixture.homePath }),
      false,
    );
  });

  it("fails closed for a default-overlay partial link failure and repairs it on retry", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const overlayHomePath = resolveActiveCodexHomeWritePath({
      env: fixture.env,
      homePath: fixture.homePath,
    });
    let failedSessionsLink = false;
    const failSessionsOnce = {
      symlink: vi.fn((sourcePath: string, targetPath: string, type?: string | null) => {
        if (path.basename(targetPath) === "sessions" && !failedSessionsLink) {
          failedSessionsLink = true;
          throw new Error("injected sessions link failure");
        }
        return symlinkSync(sourcePath, targetPath, type as "dir" | "file");
      }) as typeof symlinkSync,
      copyFile: copyFileSync,
    };

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: fixture.env,
          homePath: fixture.homePath,
          platform: "win32",
          overlayEntryLinker: failSessionsOnce,
        }),
      /injected sessions link failure/,
    );
    assert.throws(() => lstatSync(path.join(overlayHomePath, "sessions")));
    assert.ok(lstatSync(path.join(fixture.homePath, "sessions")).isDirectory());

    const retried = buildCodexProcessEnv({
      env: fixture.env,
      homePath: fixture.homePath,
      platform: "win32",
    });
    assert.strictEqual(retried.CODEX_HOME, overlayHomePath);
    assert.ok(lstatSync(path.join(overlayHomePath, "sessions")).isSymbolicLink());
    assert.ok(
      isCodexSharedContinuationStatePrepared({
        env: fixture.env,
        homePath: fixture.homePath,
      }),
    );
  });

  it("revalidates a stale migration plan and preserves a target that appears during execution", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const overlayHomePath = resolveActiveCodexHomeWritePath({
      env: fixture.env,
      homePath: fixture.homePath,
    });
    mkdirSync(overlayHomePath, { recursive: true });
    const sourceStatePath = path.join(fixture.homePath, "state_5.sqlite");
    const overlayStatePath = path.join(overlayHomePath, "state_5.sqlite");
    writeFileSync(sourceStatePath, "same-before-plan", "utf8");
    let mutated = false;
    const mutateAfterPlanningStarts = {
      symlink: vi.fn((sourcePath: string, targetPath: string, type?: string | null) => {
        const result = symlinkSync(sourcePath, targetPath, type as "dir" | "file");
        if (!mutated) {
          mutated = true;
          writeFileSync(overlayStatePath, "appeared-after-plan", "utf8");
        }
        return result;
      }) as typeof symlinkSync,
      copyFile: copyFileSync,
    };

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: fixture.env,
          homePath: fixture.homePath,
          platform: "win32",
          overlayEntryLinker: mutateAfterPlanningStarts,
        }),
      /changed during preparation/,
    );
    assert.strictEqual(readFileSync(sourceStatePath, "utf8"), "same-before-plan");
    assert.strictEqual(readFileSync(overlayStatePath, "utf8"), "appeared-after-plan");
    assert.ok(lstatSync(overlayStatePath).isFile());
  });

  it("serializes first-time continuation preparation across processes", async () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });
    const personalShadowHomePath = path.join(
      path.dirname(fixture.shadowHomePath),
      "codex-shadow-personal-lock",
    );
    mkdirSync(personalShadowHomePath, { recursive: true });
    writeFileSync(path.join(personalShadowHomePath, "auth.json"), "{}", "utf8");
    const moduleUrl = pathToFileURL(path.join(import.meta.dirname, "codexProcessEnv.ts")).href;
    const runPreparation = (accountId: string, shadowHomePath: string) =>
      new Promise<void>((resolve, reject) => {
        const script = `
          import { buildCodexProcessEnv } from ${JSON.stringify(moduleUrl)};
          buildCodexProcessEnv(${JSON.stringify({
            env: fixture.env,
            homePath: fixture.homePath,
            accountId,
            shadowHomePath,
            platform: "win32",
          })});
        `;
        const child = spawn("bun", ["-e", script], {
          cwd: import.meta.dirname,
          stdio: ["ignore", "ignore", "pipe"],
        });
        let stderr = "";
        child.stderr.setEncoding("utf8");
        child.stderr.on("data", (chunk: string) => {
          stderr += chunk;
        });
        child.once("error", reject);
        child.once("exit", (code) => {
          if (code === 0) {
            resolve();
          } else {
            reject(new Error(`Codex preparation child exited ${String(code)}: ${stderr}`));
          }
        });
      });

    await Promise.all([
      runPreparation("personal", personalShadowHomePath),
      runPreparation("work", fixture.shadowHomePath),
    ]);
    assert.ok(
      isCodexSharedContinuationStatePrepared({
        env: fixture.env,
        homePath: fixture.homePath,
        shadowHomePath: personalShadowHomePath,
        accountId: "personal",
      }),
    );
    assert.ok(
      isCodexSharedContinuationStatePrepared({
        env: fixture.env,
        homePath: fixture.homePath,
        shadowHomePath: fixture.shadowHomePath,
        accountId: "work",
      }),
    );
  });

  it.runIf(process.platform !== "win32")(
    "preserves different unreadable continuation files",
    () => {
      const fixture = makeAccountFixture({ shadowAuth: "missing" });
      const overlayHomePath = resolveActiveCodexHomeWritePath({
        env: fixture.env,
        homePath: fixture.homePath,
      });
      mkdirSync(overlayHomePath, { recursive: true });
      const sourceStatePath = path.join(fixture.homePath, "state_5.sqlite");
      const overlayStatePath = path.join(overlayHomePath, "state_5.sqlite");
      writeFileSync(sourceStatePath, "source-secret", "utf8");
      writeFileSync(overlayStatePath, "overlay-secret", "utf8");
      chmodSync(sourceStatePath, 0o000);
      chmodSync(overlayStatePath, 0o000);

      try {
        assert.throws(() =>
          buildCodexProcessEnv({
            env: fixture.env,
            homePath: fixture.homePath,
            platform: "win32",
          }),
        );
      } finally {
        chmodSync(sourceStatePath, 0o600);
        chmodSync(overlayStatePath, 0o600);
      }
      assert.strictEqual(readFileSync(sourceStatePath, "utf8"), "source-secret");
      assert.strictEqual(readFileSync(overlayStatePath, "utf8"), "overlay-secret");
    },
  );

  it.runIf(process.platform !== "win32")(
    "preserves unreadable continuation directories instead of treating them as empty",
    () => {
      const fixture = makeAccountFixture({ shadowAuth: "missing" });
      const overlayHomePath = resolveActiveCodexHomeWritePath({
        env: fixture.env,
        homePath: fixture.homePath,
      });
      const sourceSessionsPath = path.join(fixture.homePath, "sessions");
      const overlaySessionsPath = path.join(overlayHomePath, "sessions");
      mkdirSync(sourceSessionsPath, { recursive: true });
      mkdirSync(overlaySessionsPath, { recursive: true });
      writeFileSync(path.join(sourceSessionsPath, "source.jsonl"), "source", "utf8");
      writeFileSync(path.join(overlaySessionsPath, "overlay.jsonl"), "overlay", "utf8");
      chmodSync(sourceSessionsPath, 0o000);
      chmodSync(overlaySessionsPath, 0o000);

      try {
        assert.throws(() =>
          buildCodexProcessEnv({
            env: fixture.env,
            homePath: fixture.homePath,
            platform: "win32",
          }),
        );
      } finally {
        chmodSync(sourceSessionsPath, 0o700);
        chmodSync(overlaySessionsPath, 0o700);
      }
      assert.strictEqual(
        readFileSync(path.join(sourceSessionsPath, "source.jsonl"), "utf8"),
        "source",
      );
      assert.strictEqual(
        readFileSync(path.join(overlaySessionsPath, "overlay.jsonl"), "utf8"),
        "overlay",
      );
    },
  );

  it("rejects continuation links that alias another storage root", () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });
    const legacyOverlayHomePath = resolveActiveCodexHomeWritePath({
      env: fixture.env,
      homePath: fixture.homePath,
      shadowHomePath: fixture.shadowHomePath,
      accountId: "work",
    });
    const foreignSessionsPath = path.join(makeTempRoot(), "foreign-sessions");
    mkdirSync(legacyOverlayHomePath, { recursive: true });
    mkdirSync(foreignSessionsPath, { recursive: true });
    symlinkSync(foreignSessionsPath, path.join(legacyOverlayHomePath, "sessions"), "dir");

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: fixture.env,
          homePath: fixture.homePath,
          shadowHomePath: fixture.shadowHomePath,
          accountId: "work",
          platform: "win32",
        }),
      /points outside the shared source home/,
    );
  });

  it("rejects keyring auth before creating an account overlay or linking stale auth", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    writeFileSync(
      path.join(fixture.homePath, "config.toml"),
      'cli_auth_credentials_store = "keyring"\n',
      "utf8",
    );

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: fixture.env,
          homePath: fixture.homePath,
          accountId: "work",
          platform: "win32",
        }),
      /require file-backed Codex auth/,
    );
    assert.throws(() => lstatSync(path.join(fixture.env.SYNARA_HOME!, "codex-home-overlay")));
  });

  it("rejects keyring auth for the default account before creating its overlay", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    writeFileSync(
      path.join(fixture.homePath, "config.toml"),
      'cli_auth_credentials_store = "keyring"\n',
      "utf8",
    );

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: { ...fixture.env, CODEX_HOME: fixture.homePath },
          platform: "win32",
        }),
      /require file-backed Codex auth/,
    );
    assert.throws(() => lstatSync(path.join(fixture.env.SYNARA_HOME!, "codex-home-overlay")));
  });

  it("rejects selected-profile auto auth for the default account before overlay mutation", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    writeFileSync(
      path.join(fixture.homePath, "config.toml"),
      'profile = "work"\n\n[profiles.work]\ncli_auth_credentials_store = "auto"\n',
      "utf8",
    );

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: { ...fixture.env, CODEX_HOME: fixture.homePath },
          platform: "win32",
        }),
      /cli_auth_credentials_store = "auto"/,
    );
    assert.throws(() => lstatSync(path.join(fixture.env.SYNARA_HOME!, "codex-home-overlay")));
  });

  it("rejects auto auth with a shadow account before mutating the overlay", () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });
    writeFileSync(
      path.join(fixture.homePath, "config.toml"),
      'profile = "work"\n\n[profiles.work]\ncli_auth_credentials_store = "auto"\n',
      "utf8",
    );

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: fixture.env,
          homePath: fixture.homePath,
          shadowHomePath: fixture.shadowHomePath,
          accountId: "work",
          platform: "win32",
        }),
      /cli_auth_credentials_store = "auto"/,
    );
    assert.throws(() => lstatSync(path.join(fixture.env.SYNARA_HOME!, "codex-home-overlay")));
  });

  it("fails when shadow-home auth cannot be linked into the account overlay", () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });
    const firstEnv = buildCodexProcessEnv({
      env: fixture.env,
      homePath: fixture.homePath,
      shadowHomePath: fixture.shadowHomePath,
      accountId: "work",
      platform: "win32",
    });
    const overlayHomePath = firstEnv.CODEX_HOME;
    assert.ok(overlayHomePath);
    unlinkSync(path.join(overlayHomePath, "auth.json"));
    chmodSync(overlayHomePath, 0o500);
    try {
      assert.throws(
        () =>
          buildCodexProcessEnv({
            env: fixture.env,
            homePath: fixture.homePath,
            shadowHomePath: fixture.shadowHomePath,
            accountId: "work",
            platform: "win32",
          }),
        /EACCES|EPERM/,
      );
    } finally {
      chmodSync(overlayHomePath, 0o700);
    }
  });

  it("tolerates shadow homes with no auth state yet", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });

    const env = buildCodexProcessEnv({
      env: fixture.env,
      homePath: fixture.homePath,
      shadowHomePath: fixture.shadowHomePath,
      accountId: "work",
      platform: "win32",
    });

    assert.ok(env.CODEX_HOME);
  });

  it("rejects shadow-home auth state that is itself a symlink", () => {
    const fixture = makeAccountFixture({ shadowAuth: "symlink" });

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: fixture.env,
          homePath: fixture.homePath,
          shadowHomePath: fixture.shadowHomePath,
          accountId: "work",
          platform: "win32",
        }),
      /is a symlink/,
    );
  });

  it("rejects a shadow home directory that is itself a symlink", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const aliasedShadowHome = path.join(path.dirname(fixture.shadowHomePath), "codex-shadow-alias");
    symlinkSync(fixture.homePath, aliasedShadowHome);

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: fixture.env,
          homePath: fixture.homePath,
          shadowHomePath: aliasedShadowHome,
          accountId: "work",
          platform: "win32",
        }),
      /shadow home/i,
    );
  });

  it("keeps shared auth out of account overlays without a shadow home", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const sharedEnv = { ...fixture.env, CODEX_HOME: fixture.homePath };

    const env = buildCodexProcessEnv({
      env: sharedEnv,
      accountId: "work",
      platform: "win32",
    });

    const overlayHomePath = env.CODEX_HOME;
    assert.ok(overlayHomePath);
    assert.notStrictEqual(path.resolve(overlayHomePath), path.resolve(fixture.homePath));
    assert.throws(() => lstatSync(path.join(overlayHomePath, "auth.json")));
  });

  it("treats an explicit ambient Codex home as shared in overlay mode", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const sharedEnv = { ...fixture.env, CODEX_HOME: fixture.homePath };

    const env = buildCodexProcessEnv({
      env: sharedEnv,
      homePath: fixture.homePath,
      accountId: "work",
      platform: "win32",
    });

    const overlayHomePath = env.CODEX_HOME;
    assert.ok(overlayHomePath);
    assert.notStrictEqual(path.resolve(overlayHomePath), path.resolve(fixture.homePath));
    assert.throws(() => lstatSync(path.join(overlayHomePath, "auth.json")));
  });

  it("treats a parent-symlink alias of the ambient home as shared in overlay mode", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const sharedEnv = { ...fixture.env, CODEX_HOME: fixture.homePath };
    const aliasedHomePath = aliasHomeThroughParent(fixture.homePath);

    const env = buildCodexProcessEnv({
      env: sharedEnv,
      homePath: aliasedHomePath,
      accountId: "work",
      platform: "win32",
    });

    const overlayHomePath = env.CODEX_HOME;
    assert.ok(overlayHomePath);
    assert.notStrictEqual(path.resolve(overlayHomePath), path.resolve(aliasedHomePath));
    assert.throws(() => lstatSync(path.join(overlayHomePath, "auth.json")));
  });

  it("keeps account-id-only instances isolated in account overlays", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const pluginEnabledEnv = {
      ...fixture.env,
      CODEX_HOME: fixture.homePath,
    };
    writeFileSync(path.join(fixture.homePath, "config.toml"), 'model = "gpt-5.4"\n', "utf8");

    const env = buildCodexProcessEnv({
      env: pluginEnabledEnv,
      accountId: "work",
      platform: "win32",
    });

    const accountHomePath = env.CODEX_HOME;
    assert.ok(accountHomePath);
    assert.notStrictEqual(path.resolve(accountHomePath), path.resolve(fixture.homePath));
    assert.ok(lstatSync(accountHomePath).isDirectory());
    // The account overlay keeps the model/provider settings while enforcing
    // Synara's file-backed auth boundary.
    assert.match(
      readFileSync(path.join(accountHomePath, "config.toml"), "utf8"),
      /model = "gpt-5\.4"/,
    );
    // The default account uses its own non-account overlay.
    const defaultEnv = buildCodexProcessEnv({ env: pluginEnabledEnv, platform: "win32" });
    assert.ok(defaultEnv.CODEX_HOME);
    assert.notStrictEqual(defaultEnv.CODEX_HOME, fixture.homePath);
    assert.notStrictEqual(defaultEnv.CODEX_HOME, accountHomePath);
  });

  it("keeps explicit shared-home accounts isolated in account overlays", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const pluginEnabledEnv = {
      ...fixture.env,
      CODEX_HOME: fixture.homePath,
    };
    writeFileSync(path.join(fixture.homePath, "config.toml"), 'model = "gpt-5.4"\n', "utf8");

    const env = buildCodexProcessEnv({
      env: pluginEnabledEnv,
      homePath: fixture.homePath,
      accountId: "work",
      platform: "win32",
    });

    const accountHomePath = env.CODEX_HOME;
    assert.ok(accountHomePath);
    assert.notStrictEqual(path.resolve(accountHomePath), path.resolve(fixture.homePath));
    assert.ok(lstatSync(accountHomePath).isDirectory());
    assert.throws(() => lstatSync(path.join(accountHomePath, "auth.json")));
    assert.match(
      readFileSync(path.join(accountHomePath, "config.toml"), "utf8"),
      /model = "gpt-5\.4"/,
    );
  });

  it("keeps parent-symlink aliases of the shared home isolated", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const aliasedHomePath = aliasHomeThroughParent(fixture.homePath);
    const pluginEnabledEnv = {
      ...fixture.env,
      CODEX_HOME: fixture.homePath,
    };

    const env = buildCodexProcessEnv({
      env: pluginEnabledEnv,
      homePath: aliasedHomePath,
      accountId: "work",
      platform: "win32",
    });

    const accountHomePath = env.CODEX_HOME;
    assert.ok(accountHomePath);
    assert.notStrictEqual(path.resolve(accountHomePath), path.resolve(aliasedHomePath));
    assert.throws(() => lstatSync(path.join(accountHomePath, "auth.json")));
  });

  it("links dedicated account auth into an account-scoped overlay", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const dedicatedHomePath = path.join(makeTempRoot(), "codex-work-home");
    mkdirSync(dedicatedHomePath, { recursive: true });
    writeFileSync(path.join(dedicatedHomePath, "auth.json"), '{"account":"work"}', "utf8");

    const env = buildCodexProcessEnv({
      env: {
        ...fixture.env,
        CODEX_HOME: fixture.homePath,
      },
      homePath: dedicatedHomePath,
      accountId: "work",
      platform: "win32",
    });

    assert.ok(env.CODEX_HOME);
    assert.notStrictEqual(env.CODEX_HOME, dedicatedHomePath);
    assert.ok(lstatSync(path.join(env.CODEX_HOME, "auth.json")).isSymbolicLink());
    assert.strictEqual(
      path.resolve(readlinkSync(path.join(env.CODEX_HOME, "auth.json"))),
      path.resolve(path.join(dedicatedHomePath, "auth.json")),
    );
    assert.strictEqual(
      readFileSync(path.join(dedicatedHomePath, "auth.json"), "utf8"),
      '{"account":"work"}',
    );
  });

  it("rejects a symlinked shadow home", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const aliasedShadowHome = path.join(path.dirname(fixture.shadowHomePath), "codex-shadow-alias");
    symlinkSync(fixture.homePath, aliasedShadowHome);

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: {
            ...fixture.env,
            CODEX_HOME: fixture.homePath,
          },
          shadowHomePath: aliasedShadowHome,
          accountId: "work",
          platform: "win32",
        }),
      /shadow home/i,
    );
  });

  it("rejects symlinked shadow auth state", () => {
    const fixture = makeAccountFixture({ shadowAuth: "symlink" });

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: {
            ...fixture.env,
            CODEX_HOME: fixture.homePath,
          },
          shadowHomePath: fixture.shadowHomePath,
          accountId: "work",
          platform: "win32",
        }),
      /is a symlink/,
    );
  });

  it("links shadow auth into an account overlay while preserving source config", () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });
    writeFileSync(path.join(fixture.homePath, "config.toml"), 'model = "gpt-5.4"\n', "utf8");

    const env = buildCodexProcessEnv({
      env: {
        ...fixture.env,
        CODEX_HOME: fixture.homePath,
      },
      shadowHomePath: fixture.shadowHomePath,
      accountId: "work",
      platform: "win32",
    });

    assert.ok(env.CODEX_HOME);
    assert.notStrictEqual(env.CODEX_HOME, fixture.shadowHomePath);
    assert.match(
      readFileSync(path.join(env.CODEX_HOME, "config.toml"), "utf8"),
      /model = "gpt-5\.4"/,
    );
    assert.strictEqual(
      path.resolve(readlinkSync(path.join(env.CODEX_HOME, "auth.json"))),
      path.resolve(path.join(fixture.shadowHomePath, "auth.json")),
    );
    // The shadow home's own auth stays untouched.
    assert.strictEqual(
      readFileSync(path.join(fixture.shadowHomePath, "auth.json"), "utf8"),
      '{"account":"work"}',
    );
  });

  it("drops stale shared-auth symlinks when reusing the account home", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const pluginEnabledEnv = {
      ...fixture.env,
      CODEX_HOME: fixture.homePath,
    };

    // Simulate an account home a previous overlay-mode build left behind:
    // auth.json symlinked to the shared home and a plugin-disabled config.
    const overlayEnv = buildCodexProcessEnv({
      env: { ...fixture.env, CODEX_HOME: fixture.homePath },
      accountId: "work",
      platform: "win32",
    });
    const accountHomePath = overlayEnv.CODEX_HOME;
    assert.ok(accountHomePath);
    symlinkSync(path.join(fixture.homePath, "auth.json"), path.join(accountHomePath, "auth.json"));

    const env = buildCodexProcessEnv({
      env: pluginEnabledEnv,
      accountId: "work",
      platform: "win32",
    });

    assert.strictEqual(env.CODEX_HOME, accountHomePath);
    // The stale alias to the default account's auth must be gone.
    assert.throws(() => lstatSync(path.join(accountHomePath, "auth.json")));
  });

  it("mirrors private auth from an account's own dedicated home", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });

    const env = buildCodexProcessEnv({
      env: fixture.env,
      homePath: fixture.homePath,
      accountId: "work",
      platform: "win32",
    });

    const overlayHomePath = env.CODEX_HOME;
    assert.ok(overlayHomePath);
    const overlayAuthPath = path.join(overlayHomePath, "auth.json");
    assert.ok(lstatSync(overlayAuthPath).isSymbolicLink());
    assert.strictEqual(
      path.resolve(readlinkSync(overlayAuthPath)),
      path.resolve(path.join(fixture.homePath, "auth.json")),
    );
  });

  it("tracks each overlay's authoritative auth source plus its effective fallback", () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });
    const sharedEnv = { ...fixture.env, CODEX_HOME: fixture.homePath };
    const defaultTracking = resolveCodexAuthTracking({ env: sharedEnv });
    const defaultFingerprintBeforeMaterialization =
      readCodexAuthTrackingFingerprint(defaultTracking);
    assert.strictEqual(
      defaultTracking.authoritativeAuthFilePath,
      path.join(fixture.homePath, "auth.json"),
    );
    assert.match(defaultTracking.effectiveAuthFilePath ?? "", /codex-home-overlay/);
    buildCodexProcessEnv({ env: sharedEnv, platform: "win32" });
    assert.strictEqual(
      readCodexAuthTrackingFingerprint(defaultTracking),
      defaultFingerprintBeforeMaterialization,
    );

    const dedicatedHomePath = path.join(makeTempRoot(), "codex-dedicated");
    mkdirSync(dedicatedHomePath, { recursive: true });
    writeFileSync(path.join(dedicatedHomePath, "config.toml"), "", "utf8");
    const dedicatedTracking = resolveCodexAuthTracking({
      env: sharedEnv,
      homePath: dedicatedHomePath,
      accountId: "work",
    });
    assert.strictEqual(
      dedicatedTracking.authoritativeAuthFilePath,
      path.join(dedicatedHomePath, "auth.json"),
    );
    assert.match(dedicatedTracking.effectiveAuthFilePath ?? "", /codex-home-overlay/);

    const shadowTracking = resolveCodexAuthTracking({
      env: sharedEnv,
      shadowHomePath: fixture.shadowHomePath,
      accountId: "work",
    });
    assert.strictEqual(
      shadowTracking.authoritativeAuthFilePath,
      path.join(fixture.shadowHomePath, "auth.json"),
    );
    assert.match(shadowTracking.effectiveAuthFilePath ?? "", /codex-home-overlay/);

    const sharedAccountTracking = resolveCodexAuthTracking({
      env: sharedEnv,
      accountId: "work",
    });
    assert.match(sharedAccountTracking.authoritativeAuthFilePath, /codex-home-overlay/);
    assert.strictEqual(sharedAccountTracking.effectiveAuthFilePath, undefined);
  });

  it("keeps auth identity stable when a fallback copy rotates for the same account", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const sharedEnv = { ...fixture.env, CODEX_HOME: fixture.homePath };
    const sourceAuthPath = path.join(fixture.homePath, "auth.json");
    const auth = (accountId: string, token: string) =>
      JSON.stringify({
        auth_mode: "chatgpt",
        tokens: {
          account_id: accountId,
          access_token: `access-${token}`,
          refresh_token: `refresh-${token}`,
        },
      });
    writeFileSync(sourceAuthPath, auth("workspace-1", "source"), "utf8");
    const copyOnlyLinker = makeAuthCopyOnlyLinker();
    const launch = buildCodexProcessLaunchContext({
      env: sharedEnv,
      platform: "win32",
      overlayEntryLinker: copyOnlyLinker,
    });
    const effectiveAuthFilePath = launch.authTracking.effectiveAuthFilePath;
    assert.ok(effectiveAuthFilePath);
    const initialFingerprint = readCodexAuthTrackingFingerprint(launch.authTracking);

    writeFileSync(effectiveAuthFilePath, auth("workspace-1", "rotated"), "utf8");
    assert.strictEqual(readCodexAuthTrackingFingerprint(launch.authTracking), initialFingerprint);

    writeFileSync(effectiveAuthFilePath, auth("workspace-2", "swapped"), "utf8");
    assert.notStrictEqual(
      readCodexAuthTrackingFingerprint(launch.authTracking),
      initialFingerprint,
    );
  });

  it("removes an unchanged fallback copy when the authoritative account logs out", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const sharedEnv = { ...fixture.env, CODEX_HOME: fixture.homePath };
    const copyOnlyLinker = makeAuthCopyOnlyLinker();
    const firstEnv = buildCodexProcessEnv({
      env: sharedEnv,
      platform: "win32",
      overlayEntryLinker: copyOnlyLinker,
    });
    const overlayHomePath = firstEnv.CODEX_HOME;
    assert.ok(overlayHomePath);
    const overlayAuthPath = path.join(overlayHomePath, "auth.json");
    assert.ok(lstatSync(overlayAuthPath).isFile());

    unlinkSync(path.join(fixture.homePath, "auth.json"));
    buildCodexProcessEnv({
      env: sharedEnv,
      platform: "win32",
      overlayEntryLinker: copyOnlyLinker,
    });

    assert.throws(() => lstatSync(overlayAuthPath));
  });

  it("preserves overlay auth that changed after a fallback copy was created", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const sharedEnv = { ...fixture.env, CODEX_HOME: fixture.homePath };
    const copyOnlyLinker = makeAuthCopyOnlyLinker();
    const firstEnv = buildCodexProcessEnv({
      env: sharedEnv,
      platform: "win32",
      overlayEntryLinker: copyOnlyLinker,
    });
    const overlayHomePath = firstEnv.CODEX_HOME;
    assert.ok(overlayHomePath);
    const overlayAuthPath = path.join(overlayHomePath, "auth.json");
    writeFileSync(overlayAuthPath, '{"account":"independent"}', "utf8");
    unlinkSync(path.join(fixture.homePath, "auth.json"));

    buildCodexProcessEnv({
      env: sharedEnv,
      platform: "win32",
      overlayEntryLinker: copyOnlyLinker,
    });

    assert.strictEqual(readFileSync(overlayAuthPath, "utf8"), '{"account":"independent"}');
  });

  it("drops legacy shared-auth aliases from account overlays and keeps own logins", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const sharedEnv = { ...fixture.env, CODEX_HOME: fixture.homePath };

    const firstEnv = buildCodexProcessEnv({
      env: sharedEnv,
      accountId: "work",
      platform: "win32",
    });
    const overlayHomePath = firstEnv.CODEX_HOME;
    assert.ok(overlayHomePath);
    // Simulate the legacy overlay state that symlinked shared auth in.
    symlinkSync(path.join(fixture.homePath, "auth.json"), path.join(overlayHomePath, "auth.json"));

    const secondEnv = buildCodexProcessEnv({
      env: sharedEnv,
      accountId: "work",
      platform: "win32",
    });
    assert.strictEqual(secondEnv.CODEX_HOME, overlayHomePath);
    assert.throws(() => lstatSync(path.join(overlayHomePath, "auth.json")));

    // The account's own login is a real file and must survive re-preparation.
    writeFileSync(path.join(overlayHomePath, "auth.json"), '{"account":"work"}', "utf8");
    unlinkSync(path.join(fixture.homePath, "auth.json"));
    const thirdEnv = buildCodexProcessEnv({
      env: sharedEnv,
      accountId: "work",
      platform: "win32",
    });
    assert.strictEqual(thirdEnv.CODEX_HOME, overlayHomePath);
    assert.ok(lstatSync(path.join(overlayHomePath, "auth.json")).isFile());
  });
});

describe("linkOrCopyCodexOverlayEntry", () => {
  it("copies auth.json when symlink creation is unavailable", () => {
    const symlink = vi.fn(() => {
      throw new Error("symlinks unavailable");
    });
    const copyFile = vi.fn();

    linkOrCopyCodexOverlayEntry(
      {
        entryName: "auth.json",
        sourcePath: "C:\\Users\\test\\.codex\\auth.json",
        targetPath: "C:\\Users\\test\\.synara\\codex-home-overlay\\auth.json",
        type: "file",
      },
      { symlink, copyFile },
    );

    expect(symlink).toHaveBeenCalledWith(
      "C:\\Users\\test\\.codex\\auth.json",
      "C:\\Users\\test\\.synara\\codex-home-overlay\\auth.json",
      "file",
    );
    expect(copyFile).toHaveBeenCalledWith(
      "C:\\Users\\test\\.codex\\auth.json",
      "C:\\Users\\test\\.synara\\codex-home-overlay\\auth.json",
    );
  });

  it("keeps symlink failures visible for other overlay entries", () => {
    const symlink = vi.fn(() => {
      throw new Error("symlinks unavailable");
    });

    expect(() =>
      linkOrCopyCodexOverlayEntry(
        {
          entryName: "sessions",
          sourcePath: "C:\\Users\\test\\.codex\\sessions",
          targetPath: "C:\\Users\\test\\.synara\\codex-home-overlay\\sessions",
          type: "dir",
        },
        { symlink, copyFile: vi.fn() },
      ),
    ).toThrow("symlinks unavailable");
  });
});

describe("prioritizeCodexOverlayEntries", () => {
  it("prepares auth.json before entries whose symlinks may fail first", () => {
    expect(prioritizeCodexOverlayEntries(["sessions", "auth.json", "config.toml"])).toEqual([
      "auth.json",
      "sessions",
      "config.toml",
    ]);
  });
});
