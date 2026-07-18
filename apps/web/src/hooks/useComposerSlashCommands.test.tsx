import { ProjectId, ThreadId } from "@synara/contracts";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { Project, Thread } from "../types";

const { readNativeApiMock } = vi.hoisted(() => ({
  readNativeApiMock: vi.fn(),
}));

vi.mock("../nativeApi", () => ({
  readNativeApi: readNativeApiMock,
}));

import { useComposerSlashCommands } from "./useComposerSlashCommands";

type SlashCommandInput = Parameters<typeof useComposerSlashCommands>[0];
type SlashCommandController = ReturnType<typeof useComposerSlashCommands>;

function makeProject(): Project {
  return {
    id: ProjectId.makeUnsafe("project-saas"),
    kind: "project",
    name: "SaaS Project",
    remoteName: "SaaS Project",
    folderName: "saas-project",
    localName: null,
    cwd: "/workspace/saas-project",
    defaultModelSelection: { provider: "codex", model: "gpt-5.6-sol" },
    expanded: true,
    scripts: [],
  };
}

function makeThread(project: Project): Thread {
  return {
    id: ThreadId.makeUnsafe("session-saas"),
    codexThreadId: null,
    projectId: project.id,
    title: "Authoritative Session",
    modelSelection: { provider: "codex", model: "gpt-5.6-sol" },
    runtimeMode: "approval-required",
    interactionMode: "default",
    session: {
      provider: "codex",
      status: "ready",
      createdAt: "2026-07-15T00:00:00Z",
      updatedAt: "2026-07-15T00:00:00Z",
      orchestrationStatus: "ready",
    },
    messages: [],
    proposedPlans: [],
    error: null,
    createdAt: "2026-07-15T00:00:00Z",
    branch: "main",
    worktreePath: "/workspace/saas-project",
    latestTurn: null,
    turnDiffSummaries: [],
    activities: [],
  };
}

function renderSlashCommandHook(input: SlashCommandInput): SlashCommandController {
  let controller: SlashCommandController | null = null;
  function Harness() {
    controller = useComposerSlashCommands(input);
    return null;
  }
  renderToStaticMarkup(createElement(Harness));
  if (!controller) throw new Error("Slash command hook did not render.");
  return controller;
}

function makeInput(overrides: Partial<SlashCommandInput> = {}): SlashCommandInput {
  const activeProject = makeProject();
  const activeThread = makeThread(activeProject);
  return {
    activeProject,
    activeThread,
    activeRootBranch: "main",
    isServerThread: true,
    supportsFastSlashCommand: false,
    canUseLocalProviderCommands: false,
    canOfferCompactCommand: true,
    canOfferPlanCommand: true,
    canOfferReviewCommand: true,
    canOfferForkCommand: true,
    canOfferSideCommand: false,
    canOfferExportCommand: false,
    compactControlPlaneSession: vi.fn(async () => undefined),
    forkControlPlaneSession: vi.fn(async () => ThreadId.makeUnsafe("session-forked")),
    startControlPlaneReview: vi.fn(async () => undefined),
    supportsTextNativeReviewCommand: false,
    fastModeEnabled: false,
    providerNativeCommands: [],
    providerCommandDiscoveryCwd: null,
    selectedProvider: "codex",
    currentProviderModelOptions: undefined,
    selectedModelSelection: activeThread.modelSelection,
    environmentMode: null,
    runtimeMode: activeThread.runtimeMode,
    interactionMode: activeThread.interactionMode,
    threadId: activeThread.id,
    syncServerShellSnapshot: vi.fn(),
    navigateToThread: vi.fn(async () => undefined),
    handleClearConversation: vi.fn(),
    handleInteractionModeChange: vi.fn(),
    openForkTargetPicker: vi.fn(),
    openReviewTargetPicker: vi.fn(),
    setComposerDraftProviderModelOptions: vi.fn(),
    editorActions: {
      resolveActiveComposerTrigger: vi.fn(() => ({
        snapshot: { value: "", cursor: 0, expandedCursor: 0 },
        trigger: null,
      })),
      applyPromptReplacement: vi.fn(() => 0),
      clearComposerSlashDraft: vi.fn(),
      setComposerPromptValue: vi.fn(),
      scheduleComposerFocus: vi.fn(),
      setComposerHighlightedItemId: vi.fn(),
    },
    ...overrides,
  };
}

describe("useComposerSlashCommands SaaS routing", () => {
  beforeEach(() => {
    readNativeApiMock.mockReset();
    readNativeApiMock.mockImplementation(() => {
      throw new Error("SaaS advanced commands must not read the local Native API.");
    });
  });

  it("routes compact, review, and fork only through Control Plane callbacks", async () => {
    const compactControlPlaneSession = vi.fn(async () => undefined);
    const startControlPlaneReview = vi.fn(async () => undefined);
    const forkControlPlaneSession = vi.fn(async () => ThreadId.makeUnsafe("session-forked"));
    const navigateToThread = vi.fn(async () => undefined);
    const controller = renderSlashCommandHook(
      makeInput({
        compactControlPlaneSession,
        startControlPlaneReview,
        forkControlPlaneSession,
        navigateToThread,
      }),
    );

    await expect(controller.handleStandaloneSlashCommand("/compact")).resolves.toBe(true);
    await expect(controller.handleStandaloneSlashCommand("/review changes")).resolves.toBe(true);
    await expect(controller.handleStandaloneSlashCommand("/review base")).resolves.toBe(true);
    await expect(controller.handleStandaloneSlashCommand("/fork")).resolves.toBe(true);

    expect(compactControlPlaneSession).toHaveBeenCalledOnce();
    expect(startControlPlaneReview.mock.calls).toEqual([["changes"], ["base-branch"]]);
    expect(forkControlPlaneSession).toHaveBeenCalledOnce();
    expect(navigateToThread).toHaveBeenCalledWith(ThreadId.makeUnsafe("session-forked"));
    expect(readNativeApiMock).not.toHaveBeenCalled();
  });

  it("routes plan and default through interaction mode updates without local API access", async () => {
    const handleInteractionModeChange = vi.fn();
    const controller = renderSlashCommandHook(makeInput({ handleInteractionModeChange }));

    await expect(controller.handleStandaloneSlashCommand("/plan")).resolves.toBe(true);
    await expect(controller.handleStandaloneSlashCommand("/default")).resolves.toBe(true);

    expect(handleInteractionModeChange.mock.calls).toEqual([["plan"], ["default"]]);
    expect(readNativeApiMock).not.toHaveBeenCalled();
  });

  it("opens the SaaS review target picker without falling back to local discovery", async () => {
    const openReviewTargetPicker = vi.fn();
    const startControlPlaneReview = vi.fn(async () => undefined);
    const controller = renderSlashCommandHook(
      makeInput({
        openReviewTargetPicker,
        startControlPlaneReview,
      }),
    );

    await expect(controller.handleStandaloneSlashCommand("/review")).resolves.toBe(true);

    expect(openReviewTargetPicker).toHaveBeenCalledOnce();
    expect(startControlPlaneReview).not.toHaveBeenCalled();
    expect(readNativeApiMock).not.toHaveBeenCalled();
  });

  it("rejects SaaS fork local and worktree targets instead of falling back to local forking", async () => {
    const forkControlPlaneSession = vi.fn(async () => ThreadId.makeUnsafe("session-forked"));
    const navigateToThread = vi.fn(async () => undefined);
    const openForkTargetPicker = vi.fn();
    const controller = renderSlashCommandHook(
      makeInput({
        forkControlPlaneSession,
        navigateToThread,
        openForkTargetPicker,
      }),
    );

    await expect(controller.handleStandaloneSlashCommand("/fork local")).resolves.toBe(true);
    await expect(controller.handleStandaloneSlashCommand("/fork worktree")).resolves.toBe(true);

    expect(forkControlPlaneSession).not.toHaveBeenCalled();
    expect(navigateToThread).not.toHaveBeenCalled();
    expect(openForkTargetPicker).not.toHaveBeenCalled();
    expect(readNativeApiMock).not.toHaveBeenCalled();
  });
});
