import { describe, expect, it } from "vitest";

import type { AppState } from "./store";
import { syncAuthoritativeProjection } from "./store";
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
      threads: [{ ...thread, id: "local-thread" as Thread["id"] }],
      sidebarThreadSummaryById: {},
      threadsHydrated: true,
    };

    const next = syncAuthoritativeProjection(state, [project], [thread]);

    expect(next.projects.map((item) => item.id)).toEqual([project.id]);
    expect(next.threads.map((item) => item.id)).toEqual([thread.id]);
    expect(next.threadIds).toEqual([thread.id]);
    expect(next.sidebarThreadSummaryById[thread.id]?.title).toBe("Remote session");
  });
});
