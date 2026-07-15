import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { ControlPlaneSessionStreamBanner } from "./ControlPlaneSessionStreamBanner";

describe("ControlPlaneSessionStreamBanner", () => {
  it("shows an explicit reconnecting state without implying the Session completed", () => {
    const markup = renderToStaticMarkup(<ControlPlaneSessionStreamBanner status="reconnecting" />);

    expect(markup).toContain("Reconnecting to Session Events");
    expect(markup).toContain("resume from the last persisted event sequence");
    expect(markup).toContain("motion-safe:animate-spin");
    expect(markup).not.toContain("Completed");
  });

  it("uses an error notice with an explicit retry instruction", () => {
    const markup = renderToStaticMarkup(<ControlPlaneSessionStreamBanner status="error" />);

    expect(markup).toContain("Session Event stream unavailable");
    expect(markup).toContain("Reopen this Session to retry");
    expect(markup).not.toContain("motion-safe:animate-spin");
  });

  it("keeps the shared disclosure region closed for healthy streams", () => {
    const markup = renderToStaticMarkup(<ControlPlaneSessionStreamBanner status="live" />);

    expect(markup).toContain('aria-hidden="true"');
    expect(markup).toContain("Reconnecting to Session Events");
  });
});
