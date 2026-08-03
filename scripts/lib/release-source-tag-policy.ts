export type ReleaseSourceTagMode = "existing-tag" | "planned-tag" | "none";

export interface ReleaseSourceTagPolicyInput {
  readonly tag: string;
  readonly publishRelease: boolean;
  readonly enterpriseGaCandidate: boolean;
  readonly refType?: string;
  readonly refName?: string;
}

export function resolveReleaseSourceTagMode(
  input: ReleaseSourceTagPolicyInput,
): ReleaseSourceTagMode {
  if (input.refType === "tag" && input.refName === input.tag) return "existing-tag";
  if (!input.publishRelease) return "none";
  if (input.enterpriseGaCandidate && input.refType === "branch" && input.refName) {
    return "planned-tag";
  }
  throw new Error(
    input.enterpriseGaCandidate
      ? `Enterprise GA publication must start from a branch before the planned tag ${input.tag} exists.`
      : `Publishing requires the workflow ref to be the exact release tag ${input.tag}; got ${input.refType || "<none>"}/${input.refName || "<none>"}.`,
  );
}
