import { describe, expect, it } from "vitest";

import { gitCredentialTypeForRepository } from "./ProjectCredentialBindings";

describe("gitCredentialTypeForRepository", () => {
  it("accepts credential-free HTTPS repositories", () => {
    expect(gitCredentialTypeForRepository("https://github.com/acme/project.git")).toBe(
      "https_token",
    );
    expect(gitCredentialTypeForRepository("https://github.com:443/acme/project.git")).toBe(
      "https_token",
    );
  });

  it("accepts explicit ssh repositories and rejects embedded secrets", () => {
    expect(gitCredentialTypeForRepository("ssh://git@github.com/acme/project.git")).toBe("ssh_key");
    expect(gitCredentialTypeForRepository("ssh://git@github.com:2222/acme/project.git")).toBe(
      "ssh_key",
    );
    expect(gitCredentialTypeForRepository("https://token@github.com/acme/project.git")).toBeNull();
    expect(
      gitCredentialTypeForRepository("ssh://git:secret@github.com/acme/project.git"),
    ).toBeNull();
  });

  it("rejects unsupported and malformed repository forms", () => {
    expect(gitCredentialTypeForRepository("git@github.com:acme/project.git")).toBeNull();
    expect(gitCredentialTypeForRepository("http://github.com/acme/project.git")).toBeNull();
    expect(gitCredentialTypeForRepository(null)).toBeNull();
  });
});
