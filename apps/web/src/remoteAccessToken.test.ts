import { afterEach, describe, expect, it, vi } from "vitest";

import {
  captureRemoteAccessToken,
  getRemoteAccessToken,
  withRemoteAccessToken,
} from "./remoteAccessToken";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("remoteAccessToken", () => {
  it("captures a fragment token in session storage and removes it from the URL", () => {
    const values = new Map<string, string>();
    const replaceState = vi.fn();
    vi.stubGlobal("window", {
      location: {
        href: "https://synara.example.com/#token=remote-secret",
        hash: "#token=remote-secret",
        search: "",
      },
      sessionStorage: {
        getItem: (key: string) => values.get(key) ?? null,
        setItem: (key: string, value: string) => values.set(key, value),
      },
      history: { state: null, replaceState },
    });

    expect(captureRemoteAccessToken()).toBe("remote-secret");
    expect(getRemoteAccessToken()).toBe("remote-secret");
    expect(replaceState).toHaveBeenCalledWith(null, "", "https://synara.example.com/");
  });

  it("adds the stored token without overriding an explicit URL token", () => {
    vi.stubGlobal("window", {
      location: { hash: "", search: "" },
      sessionStorage: { getItem: () => "stored-secret" },
    });

    expect(withRemoteAccessToken("wss://synara.example.com/ws")).toBe(
      "wss://synara.example.com/ws?token=stored-secret",
    );
    expect(withRemoteAccessToken("wss://synara.example.com/ws?token=explicit")).toBe(
      "wss://synara.example.com/ws?token=explicit",
    );
  });
});
