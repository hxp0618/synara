import { describe, expect, it } from "vitest";

import type { OrchestrationReadModel, OrchestrationShellSnapshot } from "@synara/contracts";

import type { AppState } from "./store";
import {
  setProjectionAuthority,
  syncAuthoritativeProjection,
  syncServerReadModel,
  syncServerShellSnapshot,
} from "./store";
import { getThreadsFromState } from "./threadDerivation";
import type { Project, Thread } from "./types";

const project: Project = {
  id: "project-1" as Project["id"],
  kind: "project",
  name: "Remote project",
  remoteName: "Remote project",
  folderName: "Remote project",
  localName: null,
  cwd: "/__synara_control_plane__/tenant-1/project-1",
  defaultModelSelection: null,
  expanded: true,
  scripts: [],
};

const thread: Thread = {
  id: "session-1" as Thread["id"],
  codexThreadId: null,
  projectId: project.id,
  title: "Remote session",
  modelSelection: { provider: "codex", model: "gpt-5.6-sol" },
  runtimeMode: "full-access",
  interactionMode: "default",
  session: null,
  messages: [],
  proposedPlans: [],
  error: null,
  createdAt: "2026-07-12T00:00:00Z",
  branch: null,
  worktreePath: null,
  latestTurn: null,
  turnDiffSummaries: [],
  activities: [],
};

describe("syncAuthoritativeProjection", () => {
  it("replaces local shell entities with one normalized SaaS projection", () => {
    const state: AppState = {
      projects: [{ ...project, id: "local-project" as Project["id"], name: "Local" }],
      sidebarThreadSummaryById: {},
      threadsHydrated: true,
    };

    const next = syncAuthoritativeProjection(state, [project], [thread]);

    expect(next.projects.map((item) => item.id)).toEqual([project.id]);
    expect(getThreadsFromState(next).map((item) => item.id)).toEqual([thread.id]);
    expect(next.threadIds).toEqual([thread.id]);
    expect(next.sidebarThreadSummaryById[thread.id]?.title).toBe("Remote session");
    expect(next.projectionAuthority).toBe("control-plane");
  });

  it("rejects delayed local snapshots after Control Plane authority is active", () => {
    const authoritative = syncAuthoritativeProjection(
      {
        projects: [],
        sidebarThreadSummaryById: {},
        threadsHydrated: false,
      },
      [project],
      [thread],
    );
    const shellSnapshot = {
      snapshotSequence: 1,
      updatedAt: "2026-07-12T00:01:00Z",
      projects: [],
      threads: [],
    } satisfies OrchestrationShellSnapshot;
    const readModel = {
      snapshotSequence: 1,
      updatedAt: "2026-07-12T00:01:00Z",
      projects: [],
      threads: [],
    } satisfies OrchestrationReadModel;

    expect(syncServerShellSnapshot(authoritative, shellSnapshot)).toBe(authoritative);
    expect(syncServerReadModel(authoritative, readModel)).toBe(authoritative);
  });

  it("accepts local snapshots after auth-loss or Tenant-switch teardown demotes authority", () => {
    const authoritative = syncAuthoritativeProjection(
      {
        projects: [],
        sidebarThreadSummaryById: {},
        threadsHydrated: false,
      },
      [project],
      [thread],
    );
    const demoted = setProjectionAuthority(authoritative, "local");
    const cleared = syncAuthoritativeProjection(demoted, [], []);
    const localSnapshot = {
      snapshotSequence: 2,
      updatedAt: "2026-07-12T00:02:00Z",
      projects: [
        {
          id: "local-project-after-teardown" as OrchestrationShellSnapshot["projects"][number]["id"],
          title: "Local project",
          workspaceRoot: "/tmp/local-project",
          defaultModelSelection: null,
          createdAt: "2026-07-12T00:00:00Z",
          updatedAt: "2026-07-12T00:02:00Z",
          scripts: [],
        },
      ],
      threads: [],
    } satisfies OrchestrationShellSnapshot;

    expect(cleared.projectionAuthority).toBe("local");
    expect(syncServerShellSnapshot(cleared, localSnapshot).projects.map((item) => item.id)).toEqual(
      ["local-project-after-teardown"],
    );
  });
});
