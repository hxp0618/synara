export type CandidateEvidenceReceiptUpload = {
  readonly name: string;
  readonly sha256: string;
  readonly base64: string;
};

export type FinalReviewReceiptUpload = CandidateEvidenceReceiptUpload;

export async function readCandidateEvidenceReceipt(
  file: Pick<File, "name" | "arrayBuffer">,
): Promise<CandidateEvidenceReceiptUpload> {
  return encodeReceipt(
    file.name,
    new Uint8Array(await file.arrayBuffer()),
    32 * 1024,
    "Candidate evidence",
  );
}

export async function encodeCandidateEvidenceReceipt(
  name: string,
  bytes: Uint8Array,
): Promise<CandidateEvidenceReceiptUpload> {
  return encodeReceipt(name, bytes, 32 * 1024, "Candidate evidence");
}

export async function readFinalReviewReceipt(
  file: Pick<File, "name" | "arrayBuffer">,
): Promise<FinalReviewReceiptUpload> {
  return encodeReceipt(
    file.name,
    new Uint8Array(await file.arrayBuffer()),
    512 * 1024,
    "Final Review",
  );
}

async function encodeReceipt(
  name: string,
  bytes: Uint8Array,
  maximumBytes: number,
  label: string,
): Promise<CandidateEvidenceReceiptUpload> {
  if (bytes.byteLength < 1 || bytes.byteLength > maximumBytes) {
    throw new Error(`${label} receipt must be between 1 byte and ${maximumBytes / 1024} KiB.`);
  }
  const digestInput = new Uint8Array(bytes.byteLength);
  digestInput.set(bytes);
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", digestInput));
  const sha256 = `sha256:${Array.from(digest, (value) => value.toString(16).padStart(2, "0")).join("")}`;
  let binary = "";
  for (const value of bytes) binary += String.fromCharCode(value);
  return { name, sha256, base64: btoa(binary) };
}
