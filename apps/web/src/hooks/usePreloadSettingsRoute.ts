import { useEffect } from "react";
import { useRouter } from "@tanstack/react-router";

import type { AppRouter } from "../router";

type SettingsRouteChunkLoader = Pick<AppRouter, "loadRouteChunk" | "routesById">;

export function preloadSettingsRouteChunk(router: SettingsRouteChunkLoader) {
  return router.loadRouteChunk(router.routesById["/_chat/settings"]);
}

/** Warms the code-split settings route chunk once the browser is idle.
 *
 *  Settings is reached through programmatic `navigate()` calls (sidebar gear,
 *  keyboard shortcut), so the router's intent-based preloading never fires for
 *  it — without this, the first open pays the chunk download/parse cost.
 */
export function usePreloadSettingsRoute() {
  const router = useRouter();

  useEffect(() => {
    const preload = () => {
      preloadSettingsRouteChunk(router)?.catch(() => {
        // Chunk warming is best-effort; navigation loads it on demand.
      });
    };

    if (typeof requestIdleCallback === "function") {
      const idleCallbackId = requestIdleCallback(preload, { timeout: 5000 });
      return () => cancelIdleCallback(idleCallbackId);
    }
    const timeoutId = setTimeout(preload, 1500);
    return () => clearTimeout(timeoutId);
  }, [router]);
}
