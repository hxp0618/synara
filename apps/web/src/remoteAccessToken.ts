// FILE: remoteAccessToken.ts
// Purpose: Carries the Phase 1 remote-server access token from a URL fragment
// into WebSocket and authenticated asset requests without leaving it visible.

const REMOTE_ACCESS_TOKEN_STORAGE_KEY = "synara.remoteAccessToken";

function tokenFromLocation(): string | null {
  if (typeof window === "undefined") return null;

  const hash = typeof window.location.hash === "string" ? window.location.hash : "";
  const hashToken = new URLSearchParams(hash.startsWith("#") ? hash.slice(1) : hash).get("token");
  if (hashToken?.trim()) return hashToken.trim();

  // Keep compatibility with early remote-access links while preferring the
  // fragment form, which is not sent to the HTTP server or reverse proxy.
  const search = typeof window.location.search === "string" ? window.location.search : "";
  const queryToken = new URLSearchParams(search).get("token");
  return queryToken?.trim() || null;
}

function storedToken(): string | null {
  if (typeof window === "undefined") return null;
  try {
    return window.sessionStorage?.getItem(REMOTE_ACCESS_TOKEN_STORAGE_KEY)?.trim() || null;
  } catch {
    return null;
  }
}

export function captureRemoteAccessToken(): string | null {
  const token = tokenFromLocation();
  if (!token) return storedToken();

  let persisted = false;
  try {
    window.sessionStorage?.setItem(REMOTE_ACCESS_TOKEN_STORAGE_KEY, token);
    persisted = window.sessionStorage?.getItem(REMOTE_ACCESS_TOKEN_STORAGE_KEY) === token;
  } catch {
    // Storage can be unavailable in hardened/private browser contexts. The
    // token stays in the fragment so later transport construction can read it.
  }

  if (!persisted) return token;

  try {
    const url = new URL(window.location.href);
    url.searchParams.delete("token");
    const hashParams = new URLSearchParams(url.hash.startsWith("#") ? url.hash.slice(1) : url.hash);
    hashParams.delete("token");
    const nextHash = hashParams.toString();
    url.hash = nextHash ? `#${nextHash}` : "";
    window.history?.replaceState(window.history.state, "", url.toString());
  } catch {
    // The desktop test bridge and non-standard origins do not always expose a
    // fully parseable location. Token propagation remains functional.
  }

  return token;
}

export function getRemoteAccessToken(): string | null {
  return storedToken() ?? tokenFromLocation();
}

export function withRemoteAccessToken(rawUrl: string): string {
  const token = getRemoteAccessToken();
  if (!token) return rawUrl;

  try {
    const url = new URL(rawUrl);
    if (!url.searchParams.has("token")) {
      url.searchParams.set("token", token);
    }
    return url.toString();
  } catch {
    return rawUrl;
  }
}
