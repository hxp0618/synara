const MAX_INTERACTION_REQUEST_ID_BYTES = 200;

export function providerInteractionRequestId(
  provider: "codex" | "claude",
  generation: number | undefined,
  kind: string | undefined,
  nativeId: string | number,
  disambiguator?: number,
): string {
  const prefix =
    [
      provider,
      ...(generation === undefined ? [] : [`generation-${generation}`]),
      ...(kind === undefined ? [] : [kind]),
    ].join(":") + ":";
  const suffix = disambiguator === undefined ? "" : `:${disambiguator}`;
  const nativeIdByteLimit =
    MAX_INTERACTION_REQUEST_ID_BYTES - Buffer.byteLength(prefix) - Buffer.byteLength(suffix);
  if (nativeIdByteLimit < 0) {
    throw new Error("Provider interaction request ID namespace exceeds the wire limit.");
  }
  return `${prefix}${truncateUtf8(String(nativeId), nativeIdByteLimit)}${suffix}`;
}

function truncateUtf8(value: string, maximumBytes: number): string {
  if (Buffer.byteLength(value) <= maximumBytes) return value;
  let result = "";
  let usedBytes = 0;
  for (const character of value) {
    const characterBytes = Buffer.byteLength(character);
    if (usedBytes + characterBytes > maximumBytes) break;
    result += character;
    usedBytes += characterBytes;
  }
  return result;
}
