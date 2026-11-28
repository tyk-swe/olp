import { snapshotNativeJSON, stringifyNativeJSON } from '#lib/nativeJson.js';
import type { MutationIntent } from '#lib/editing/mutationIntent.js';
import {
  activateProvider,
  createProvider,
  isRefusedProblem,
  updateProvider
} from '#lib/features/providers/providers.js';
import type {
  Provider,
  ProviderCredential
} from '#lib/domain/providers/api.js';
import { rotateProviderCredential } from '#lib/features/providers/credentials.js';

export type ConnectionInput = Parameters<typeof createProvider>[0];

export type ConnectionProgress = { input: ConnectionInput } & (
  | { phase: 'create'; uncertain: boolean }
  | {
      phase: 'update';
      provider: Pick<Provider, 'id' | 'etag'>;
      uncertain: boolean;
    }
  | {
      phase: 'credential';
      provider: Pick<Provider, 'id' | 'etag'>;
      status: 'ready' | 'refused' | 'uncertain';
    }
  | { phase: 'saved'; id: string }
);

export type ActivationProgress = { input: Pick<Provider, 'id' | 'etag'> } & (
  | { phase: 'pending'; uncertain: boolean }
  | { phase: 'committed'; generation: number }
);

export function isLiveGrant(
  credentials: ProviderCredential[] | undefined,
  id: string | null | undefined,
  pluginDigest: string | undefined
): boolean {
  const credential = credentials?.find((candidate) => candidate.id === id);
  return Boolean(
    credential?.grant &&
    !credential.revoked_at &&
    !credential.grant.lapsed_at &&
    credential.grant.plugin_digest === pluginDigest
  );
}

export function resolveConnectionProgress(
  input: ConnectionInput,
  previous: ConnectionProgress | undefined,
  existing: Pick<Provider, 'id' | 'etag'> | null
): { progress?: ConnectionProgress; error?: string } {
  const changed =
    previous &&
    stringifyNativeJSON(previous.input) !== stringifyNativeJSON(input);
  const credentialCorrection =
    previous?.phase === 'credential' &&
    previous.status === 'refused' &&
    stringifyNativeJSON({
      ...previous.input,
      credential: undefined,
      credential_reference: undefined
    }) ===
      stringifyNativeJSON({
        ...input,
        credential: undefined,
        credential_reference: undefined
      });
  if (changed && !credentialCorrection) {
    return {
      error:
        previous.phase === 'saved'
          ? 'Connection saved. Retry the original fields to refresh and test it before editing, or leave this wizard.'
          : 'The previous submission may already have saved. Retry its original fields before editing, or leave this wizard.'
    };
  }
  const progress: ConnectionProgress = credentialCorrection
    ? { ...previous, input: snapshotNativeJSON(input), status: 'ready' }
    : (previous ??
      (existing
        ? {
            phase: 'update',
            input: snapshotNativeJSON(input),
            provider: { id: existing.id, etag: existing.etag },
            uncertain: false
          }
        : {
            phase: 'create',
            input: snapshotNativeJSON(input),
            uncertain: false
          }));
  return { progress };
}

function connectionSaved(
  input: ConnectionInput,
  id: string
): Extract<ConnectionProgress, { phase: 'saved' }> {
  return {
    phase: 'saved',
    id,
    input: snapshotNativeJSON({
      ...input,
      credential: input.credential === undefined ? undefined : ''
    })
  };
}

type SaveDeps = {
  createIntent: MutationIntent;
  rotateIntent: MutationIntent;
  isCurrent: () => boolean;
  setProgress: (next: ConnectionProgress | undefined) => void;
  setNotice: (notice: string) => void;
};

async function trackWrite<T>(
  progress: Extract<ConnectionProgress, { phase: 'create' | 'update' }>,
  { createIntent, isCurrent, setProgress }: SaveDeps,
  write: () => Promise<T>
): Promise<T> {
  try {
    return await write();
  } catch (error) {
    if (isCurrent()) {
      if (!isRefusedProblem(error))
        setProgress({ ...progress, uncertain: true });
      else if (!progress.uncertain) {
        setProgress(undefined);
        createIntent.reset();
      }
    }
    throw error;
  }
}

export async function saveProviderConnection(
  progress: ConnectionProgress,
  deps: SaveDeps
): Promise<Extract<ConnectionProgress, { phase: 'saved' }> | undefined> {
  if (progress.phase === 'saved') return progress;
  if (progress.phase === 'update') {
    const updated = await trackWrite(progress, deps, () =>
      updateProvider(progress.provider.id, progress.provider.etag, {
        name: progress.input.name,
        configuration: progress.input.configuration
      })
    );
    if (!deps.isCurrent()) return;
    const next: ConnectionProgress = {
      phase: 'credential',
      input: progress.input,
      provider: { id: updated.id, etag: updated.etag },
      status: 'ready'
    };
    deps.setProgress(next);
    deps.setNotice('Connection configuration saved.');
    return saveProviderConnection(next, deps);
  }
  if (progress.phase === 'credential') {
    const credential =
      progress.input.credential_reference ?? progress.input.credential;
    if (credential) {
      try {
        await deps.rotateIntent.submit(
          [progress.provider, credential] as const,
          (input, requestId) => rotateProviderCredential(...input, requestId)
        );
      } catch (error) {
        if (deps.isCurrent()) {
          const refused =
            isRefusedProblem(error) && progress.status !== 'uncertain';
          deps.setProgress({
            ...progress,
            status: refused ? 'refused' : 'uncertain'
          });
          if (refused) deps.rotateIntent.reset();
        }
        throw error;
      }
    }
    if (!deps.isCurrent()) return;
    const saved = connectionSaved(progress.input, progress.provider.id);
    deps.setProgress(saved);
    return saved;
  }
  const request = deps.createIntent.prepare(progress.input);
  const id = await trackWrite(progress, deps, () =>
    createProvider(request.input, request.requestId)
  );
  if (!deps.isCurrent()) return;
  deps.createIntent.reset();
  const saved = connectionSaved(progress.input, id);
  deps.setProgress(saved);
  return saved;
}

export async function commitProviderActivation(
  progress: ActivationProgress,
  deps: {
    activateIntent: MutationIntent;
    isCurrent: () => boolean;
    setProgress: (next: ActivationProgress | undefined) => void;
  }
): Promise<Extract<ActivationProgress, { phase: 'committed' }> | undefined> {
  if (progress.phase === 'committed') return progress;
  try {
    const generation = await deps.activateIntent.submit(
      progress.input,
      (input, requestId) => activateProvider(input, requestId)
    );
    if (!deps.isCurrent()) return;
    const committed = {
      phase: 'committed' as const,
      input: progress.input,
      generation
    };
    deps.setProgress(committed);
    return committed;
  } catch (error) {
    if (deps.isCurrent()) {
      if (isRefusedProblem(error) && !progress.uncertain) {
        deps.setProgress(undefined);
        deps.activateIntent.reset();
      } else if (!isRefusedProblem(error)) {
        deps.setProgress({ ...progress, uncertain: true });
      }
    }
    throw error;
  }
}
