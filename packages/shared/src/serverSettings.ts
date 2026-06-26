import {
  DEFAULT_MODEL_BY_PROVIDER,
  type ModelSelection,
  type ProviderKind,
  type ServerSettings,
  type ServerSettingsPatch,
} from "@t3tools/contracts";
import { deepMerge, type DeepPartial } from "./Struct";
import { deriveProviderInstances } from "./providerInstances";

function defaultModelForProvider(provider: ProviderKind): string {
  return provider === "pi" ? "openai/gpt-5.5" : DEFAULT_MODEL_BY_PROVIDER[provider];
}

function shouldReplaceTextGenerationModelSelection(
  patch: ServerSettingsPatch["textGenerationModelSelection"] | undefined,
): boolean {
  return Boolean(
    patch &&
    (patch.provider !== undefined || patch.instanceId !== undefined || patch.model !== undefined),
  );
}

export function applyServerSettingsPatch(
  current: ServerSettings,
  patch: ServerSettingsPatch,
): ServerSettings {
  const selectionPatch = patch.textGenerationModelSelection;
  const merged = deepMerge(current, patch as DeepPartial<ServerSettings>);
  const next: ServerSettings =
    patch.providerInstances !== undefined
      ? { ...merged, providerInstances: patch.providerInstances }
      : merged;
  if (!selectionPatch) {
    return next;
  }

  const patchedInstanceId =
    selectionPatch.instanceId ??
    (selectionPatch.provider &&
    selectionPatch.provider !== current.textGenerationModelSelection.provider
      ? selectionPatch.provider
      : current.textGenerationModelSelection.instanceId);
  const patchedInstance =
    patchedInstanceId !== undefined
      ? deriveProviderInstances(next).find((instance) => instance.instanceId === patchedInstanceId)
      : undefined;
  const provider =
    patchedInstance?.driver ??
    selectionPatch.provider ??
    current.textGenerationModelSelection.provider;
  const instanceId = patchedInstance?.instanceId ?? patchedInstanceId;
  const providerChanged = provider !== current.textGenerationModelSelection.provider;
  const model =
    selectionPatch.model ??
    (providerChanged
      ? defaultModelForProvider(provider)
      : current.textGenerationModelSelection.model);
  const options = shouldReplaceTextGenerationModelSelection(selectionPatch)
    ? selectionPatch.options
    : (selectionPatch.options ?? current.textGenerationModelSelection.options);

  return {
    ...next,
    textGenerationModelSelection: {
      provider,
      ...(instanceId !== undefined ? { instanceId } : {}),
      model,
      ...(options !== undefined ? { options } : {}),
    } as ModelSelection,
  };
}
