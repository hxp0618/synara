// FILE: usePreloadSettingsRoute.test.ts
// Purpose: Keeps idle Settings warming isolated from route-match preload state.
// Layer: Hook helper unit tests

import { describe, expect, it, vi } from "vitest";

import type { AppRouter } from "../router";
import { preloadSettingsRouteChunk } from "./usePreloadSettingsRoute";

describe("preloadSettingsRouteChunk", () => {
  it("loads only the generated Settings route chunk", async () => {
    const settingsRoute = { id: "/_chat/settings" };
    const loadRouteChunk = vi.fn().mockResolvedValue(undefined);
    const router = {
      loadRouteChunk,
      routesById: {
        "/_chat/settings": settingsRoute,
      },
    } as unknown as Pick<AppRouter, "loadRouteChunk" | "routesById">;

    await preloadSettingsRouteChunk(router);

    expect(loadRouteChunk).toHaveBeenCalledOnce();
    expect(loadRouteChunk).toHaveBeenCalledWith(settingsRoute);
  });
});
