// FILE: chatProjects.test.ts
// Purpose: Verifies home chat-container project recognition across new and legacy roots.

import { ProjectId } from "@t3tools/contracts";
import { afterEach, describe, expect, it, vi } from "vitest";

import { useStore } from "../store";
import { ensureHomeChatProject, isHomeChatContainerProject } from "./chatProjects";

afterEach(() => {
  vi.unstubAllGlobals();
  useStore.setState({
    projects: [],
    threadIds: [],
    threads: [],
  });
});

describe("isHomeChatContainerProject", () => {
  it("matches the managed Documents/Synara general-chat root used by older drafts", () => {
    expect(
      isHomeChatContainerProject(
        {
          cwd: "/Users/tester/Documents/Synara",
          kind: "chat",
          name: "Home",
          remoteName: "Home",
        },
        {
          homeDir: "/Users/tester",
          chatWorkspaceRoot: "/Users/tester/Documents/Synara",
        },
      ),
    ).toBe(true);
  });

  it("matches Codex-style date/slug chat workspaces under Documents/Synara", () => {
    expect(
      isHomeChatContainerProject(
        {
          cwd: "/Users/tester/Documents/Synara/2026-06-11/yes-it-takes-all-the-skills",
          kind: "chat",
          name: "Yes it takes",
          remoteName: "Yes it takes",
        },
        {
          homeDir: "/Users/tester",
          chatWorkspaceRoot: "/Users/tester/Documents/Synara",
        },
      ),
    ).toBe(true);
  });

  it("keeps recognizing the legacy home-directory chat container during migration", () => {
    expect(
      isHomeChatContainerProject(
        {
          cwd: "/Users/tester",
          kind: "chat",
          name: "Home",
          remoteName: "Home",
        },
        {
          homeDir: "/Users/tester",
          chatWorkspaceRoot: "/Users/tester/Documents/Synara",
        },
      ),
    ).toBe(true);
  });

  it("does not classify ordinary projects under Documents/Synara as home chat containers", () => {
    expect(
      isHomeChatContainerProject(
        {
          cwd: "/Users/tester/Documents/Synara",
          kind: "project",
          name: "Synara",
          remoteName: "Synara",
        },
        {
          homeDir: "/Users/tester",
          chatWorkspaceRoot: "/Users/tester/Documents/Synara",
        },
      ),
    ).toBe(false);
  });

  it("does not classify ordinary projects under date/slug chat folders", () => {
    expect(
      isHomeChatContainerProject(
        {
          cwd: "/Users/tester/Documents/Synara/2026-06-11/yes-it-takes-all-the-skills",
          kind: "project",
          name: "yes-it-takes-all-the-skills",
          remoteName: "yes-it-takes-all-the-skills",
        },
        {
          homeDir: "/Users/tester",
          chatWorkspaceRoot: "/Users/tester/Documents/Synara",
        },
      ),
    ).toBe(false);
  });

  it("recovers an existing Home project id when creation races a stale duplicate", async () => {
    const existingProjectId = ProjectId.makeUnsafe("project-home-existing");
    const dispatchCommand = vi.fn(async () => {
      throw new Error(
        `Orchestration command invariant failed (project.create): Project '${existingProjectId}' already uses workspace root '/Users/tester'.`,
      );
    });
    vi.stubGlobal("window", {
      nativeApi: {
        orchestration: {
          dispatchCommand,
        },
      },
    });

    const projectId = await ensureHomeChatProject({
      homeDir: "/Users/tester",
      chatWorkspaceRoot: "/Users/tester/Documents/Synara",
    });

    expect(projectId).toBe(existingProjectId);
    expect(dispatchCommand).toHaveBeenCalledWith(
      expect.objectContaining({
        type: "project.create",
        kind: "chat",
        workspaceRoot: "/Users/tester",
      }),
    );
  });
});
