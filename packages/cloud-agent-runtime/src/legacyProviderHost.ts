/**
 * Transitional Provider Host compatibility surface.
 *
 * This subpath is intentionally excluded from the default Runtime ABI. New
 * hosts use the app-neutral root entrypoint; only the first-party Provider
 * packages and the temporary `@synara/provider-host` wrapper may depend on it.
 */
export {
  capabilityMapForProvider,
  createProviderHostProtocolHandler,
  providerHostDescriptor,
  runProviderHostProtocolV2,
  type CodexVersionProbeResult,
  type ProviderHostDescriptorOptions,
} from "./protocol";
export {
  createRedactor,
  hasAuthoritativeResumeData,
  hasResumeSupplementalMetadata,
  nativeResumeContinuationPrompt,
  providerEnvironment,
  providerProcessEnvironment,
  readRunnerCredential,
  reconstructedPrompt,
  runProviderHost,
  startProviderHostRun,
  validateRunnerInput,
  type ProviderPrimaryOperation,
  type ProviderReviewTarget,
  type ProviderRunController,
  type ProviderRunOptions,
  type ResumeSnapshot,
  type ResumeSnapshotMessage,
  type RunnerCredential,
  type RunnerInput,
  type RunnerMessage,
} from "./providerHost";
export { normalizeRuntimeEventV2, type RuntimeEventV2WirePayload } from "./runtimeEventV2";
export {
  createLegacyProviderPlugin,
  type LegacyProviderPluginOptions,
  type PortableProviderKind,
} from "./providerPlugin";
