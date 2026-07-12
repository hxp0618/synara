import { useCallback } from "react";

import { ensureHomeChatProject } from "../lib/chatProjects";
import { startContainerChat, type StartContainerChatResult } from "../lib/startContainerChat";
import { useWorkspaceStore } from "../workspaceStore";
import { useHandleNewThread } from "./useHandleNewThread";
import { useControlPlane } from "../controlPlaneContext";

export function useHandleNewChat() {
  const homeDir = useWorkspaceStore((state) => state.homeDir);
  const chatWorkspaceRoot = useWorkspaceStore((state) => state.chatWorkspaceRoot);
  const controlPlane = useControlPlane();
  const { handleNewThread, projects } = useHandleNewThread();

  const handleNewChat = useCallback(
    async (options?: { fresh?: boolean }): Promise<StartContainerChatResult> => {
      if (controlPlane.isAuthoritative) {
        const project = projects[0];
        if (!project) {
          return {
            ok: false,
            error: "Create a SaaS Project from the sidebar before starting a chat.",
          };
        }
        await handleNewThread(project.id, {
          ...(options?.fresh ? { fresh: true } : {}),
          envMode: "local",
          worktreePath: null,
        });
        return { ok: true };
      }
      if (!homeDir) {
        return {
          ok: false,
          error: "Home folder is not available yet.",
        };
      }

      return startContainerChat({
        ensureProjectId: () => ensureHomeChatProject({ homeDir, chatWorkspaceRoot }),
        handleNewThread,
        fresh: options?.fresh,
        errorLabel: "Unable to prepare a new chat.",
      });
    },
    [chatWorkspaceRoot, controlPlane.isAuthoritative, handleNewThread, homeDir, projects],
  );

  return { handleNewChat };
}
