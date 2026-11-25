import type { QueryClient } from '@tanstack/svelte-query';
import { errorMessage } from '#lib/management/http.js';
import { nativeObject, parseNativeJSON } from '#lib/nativeJson.js';
import type { Action } from '#lib/editing/action.svelte.js';
import type { MutationIntentGroup } from '#lib/editing/editorSession.svelte.js';
import { readResult } from '#lib/editing/result.js';
import { networkCredentialPath } from './configurationDraft.svelte';
import {
  activateProvider,
  disableProvider,
  isProviderInUse,
  probeProvider,
  restoreProviderAsDraft,
  updateProvider,
  type Provider
} from './providers';
import {
  certifyProviderModel,
  compareProviderListing,
  declareProviderModels,
  discoverProviderModels,
  setProviderModel,
  type CapabilityCertification,
  type CapabilityDeclaration,
  type ProviderKindCapability,
  type ProviderListingComparison,
  type ProviderModelPage
} from './models';
import {
  revokeProviderCredential,
  rotateProviderCredential,
  type ExternalCredentialReference,
  type ProviderCredential
} from './credentials';
import {
  diffProviderRevisions,
  restoreProviderRevision,
  type ProviderRevisionDiff
} from './revisions';
import { acceptCatalogSuggestions } from './catalog';
import {
  createNetworkCredential,
  revokeNetworkCredential
} from './providerProfiles';
import { providerKeys } from './providerKeys';
import type { ProviderChangeReceipt } from './providerChanges';
import {
  buildUpdateProviderInput,
  certificationPrerequisiteReady,
  disableNotice,
  parseManualModelNames,
  probeSummary,
  type ProviderEditValues
} from './providerEditor';

export const providerEditorIntentNames = [
  'activate',
  'disable',
  'restore',
  'revision',
  'stage',
  'revoke',
  'networkCreate',
  'networkRevoke',
  'catalog'
] as const;

export type ProviderEditorIntentGroup = MutationIntentGroup<
  typeof providerEditorIntentNames
>;

export type ProviderEditorContext = {
  readonly queryClient: QueryClient;
  readonly intents: ProviderEditorIntentGroup;
  readonly canManage: () => boolean;
  readonly providerId: () => string;
  readonly current: () => Provider | undefined;
  readonly providerSpec: () => ProviderKindCapability | undefined;
  readonly pending: () => ProviderChangeReceipt | null;
  readonly dirty: () => boolean;
  readonly snapshotEtag: () => string | null;
  readonly editValues: () => ProviderEditValues;
  readonly editVersion: () => number;
  readonly markDraftSaved: (etag: string, stillDirty: boolean) => void;
  readonly setReferenceConflict: (message: string) => void;
  readonly reportError: (message: string) => void;
  readonly touch: () => void;
  readonly resetModelPage: () => void;
  readonly run: Action['run'];
  readonly observe: (
    receipt: ProviderChangeReceipt,
    isCurrent: () => boolean
  ) => Promise<void>;
};

const activation = (id: string, notice: string): ProviderChangeReceipt => ({
  change: { kind: 'activation', id },
  notice
});

export function createProviderEditorActions(ctx: ProviderEditorContext) {
  const readyForWrite = () => {
    const current = ctx.current();
    return current && ctx.canManage() && !ctx.pending() ? current : undefined;
  };

  return {
    saveDraft: async () => {
      const current = readyForWrite();
      const spec = ctx.providerSpec();
      if (!current || !spec) return false;
      const version = ctx.editVersion();
      return ctx.run(
        async () => {
          const snapshotEtag = ctx.snapshotEtag();
          if (!snapshotEtag)
            throw new Error('Reload the provider before saving.');
          return updateProvider(
            current.id,
            snapshotEtag,
            buildUpdateProviderInput(ctx.editValues(), spec)
          );
        },
        async (updated, isCurrent) => {
          const stillDirty = ctx.editVersion() !== version;
          ctx.markDraftSaved(updated.etag, stillDirty);
          ctx.intents.reset('activate', 'disable', 'restore');
          ctx.queryClient.setQueriesData<ProviderModelPage>(
            { queryKey: providerKeys.modelsOf(current.id) },
            (page) => (page ? { ...page, provider: updated } : page)
          );
          ctx.resetModelPage();
          await ctx.observe(
            activation(
              current.id,
              stillDirty
                ? 'Draft saved. You have additional unsaved changes.'
                : 'Provider draft settings saved.'
            ),
            isCurrent
          );
        },
        { key: 'save' }
      );
    },

    probe: async () => {
      const current = readyForWrite();
      if (!current || ctx.dirty()) return false;
      return ctx.run(
        () => probeProvider(current),
        async ({ result }, isCurrent) => {
          if (!result.succeeded) throw new Error(result.detail);
          await ctx.observe(
            activation(
              current.id,
              `Connection succeeded: ${probeSummary(result)}`
            ),
            isCurrent
          );
        },
        { key: 'detail-probe' }
      );
    },

    activate: async () => {
      const current = readyForWrite();
      if (!current || ctx.dirty()) return false;
      return ctx.run(
        async () => {
          const submission = ctx.intents.activate.prepare(current.id, () => ({
            id: current.id,
            etag: current.etag
          }));
          return activateProvider(submission.input, submission.requestId);
        },
        async (generation, isCurrent) => {
          ctx.intents.activate.reset();
          await ctx.observe(
            activation(
              current.id,
              `Activated in runtime generation ${generation}.`
            ),
            isCurrent
          );
        },
        { key: 'detail-activate' }
      );
    },

    disable: async () => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run(
        async () => {
          const submission = ctx.intents.disable.prepare(current.id, () => ({
            id: current.id,
            etag: current.etag
          }));
          const disabled = await readResult(() =>
            disableProvider(submission.input, submission.requestId)
          );
          if (!disabled.ok && !isProviderInUse(disabled.error))
            throw disabled.error;
          return disabled;
        },
        async (disabled, isCurrent) => {
          if (!disabled.ok) {
            ctx.setReferenceConflict(errorMessage(disabled.error));
            return;
          }
          ctx.intents.disable.reset();
          await ctx.observe(
            activation(current.id, disableNotice(disabled.value)),
            isCurrent
          );
        },
        { key: 'detail-disable' }
      );
    },

    restoreDraft: async () => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run(
        async () => {
          const submission = ctx.intents.restore.prepare(current.id, () => ({
            id: current.id,
            etag: current.etag
          }));
          return restoreProviderAsDraft(submission.input, submission.requestId);
        },
        async (_restored, isCurrent) => {
          ctx.intents.restore.reset();
          await ctx.observe(
            activation(
              current.id,
              'Provider restored as a draft. Stored capabilities are declared again: re-certify and test before activation.'
            ),
            isCurrent
          );
        },
        { key: 'detail-restore-draft' }
      );
    },

    stageCredential: async (
      credential: string | ExternalCredentialReference
    ) => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run(
        async () => {
          const submission = ctx.intents.stage.prepare(
            { providerId: current.id, credential },
            () => [{ id: current.id, etag: current.etag }, credential] as const
          );
          return rotateProviderCredential(
            ...submission.input,
            submission.requestId
          );
        },
        async (_rotated, isCurrent) => {
          ctx.intents.stage.reset();
          ctx.resetModelPage();
          await ctx.observe(
            activation(
              current.id,
              'Credential version staged. Test and activate the provider to publish it; the current runtime credential remains live until then.'
            ),
            isCurrent
          );
        },
        { key: 'rotate-credential' }
      );
    },

    revokeCredential: async (credential: ProviderCredential) => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run(
        async () => {
          const submission = ctx.intents.revoke.prepare(
            { providerId: current.id, credentialId: credential.id },
            () =>
              [{ id: current.id, etag: current.etag }, credential.id] as const
          );
          return revokeProviderCredential(
            ...submission.input,
            submission.requestId
          );
        },
        async (_revoked, isCurrent) => {
          ctx.intents.revoke.reset();
          await ctx.observe(
            activation(
              current.id,
              `Credential version ${credential.version} revoked.`
            ),
            isCurrent
          );
        },
        { key: `revoke-${credential.id}` }
      );
    },

    restoreRevision: async (revisionId: string, revision: number) => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run(
        async () => {
          const submission = ctx.intents.revision.prepare(
            { providerId: current.id, revisionId },
            () => ({
              provider: { id: current.id, etag: current.etag },
              revisionId
            })
          );
          return restoreProviderRevision(
            submission.input.provider,
            submission.input.revisionId,
            submission.requestId
          );
        },
        async (_restored, isCurrent) => {
          ctx.intents.revision.reset();
          ctx.resetModelPage();
          await ctx.observe(
            activation(
              current.id,
              `Revision ${revision} restored as a new draft. No historical credential was restored; the current credential selection was preserved. Test and certify before activation.`
            ),
            isCurrent
          );
        },
        { key: 'revision-restore' }
      );
    },

    compareListing: async (
      accept: (comparison: ProviderListingComparison) => void
    ) => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run(() => compareProviderListing(current.id), accept, {
        key: 'listing-comparison'
      });
    },

    compareRevisions: async (
      from: string,
      to: string,
      accept: (diff: ProviderRevisionDiff) => void
    ) => {
      const current = ctx.current();
      if (!current) return false;
      return ctx.run(
        () => diffProviderRevisions(current.id, from, to),
        accept,
        { key: 'revision-diff' }
      );
    },

    discoverModels: async () => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run(
        () => discoverProviderModels(current),
        async (updated, isCurrent) => {
          ctx.resetModelPage();
          await ctx.observe(
            activation(
              current.id,
              `${updated.model_count} model${updated.model_count === 1 ? '' : 's'} discovered. Review capabilities before activation.`
            ),
            isCurrent
          );
        },
        { key: 'detail-discover' }
      );
    },

    declareModels: async (input: string) => {
      const current = readyForWrite();
      if (!current) return false;
      const names = parseManualModelNames(input);
      if (!names.length) {
        ctx.reportError('Enter at least one upstream model identifier.');
        return false;
      }
      return ctx.run(
        () => declareProviderModels(current, names),
        async (updated, isCurrent) => {
          ctx.resetModelPage();
          await ctx.observe(
            activation(
              current.id,
              `${updated.model_count} manually declared model${updated.model_count === 1 ? '' : 's'} ready for capability review.`
            ),
            isCurrent
          );
        },
        { key: 'detail-declare' }
      );
    },

    reviewModel: async (
      provider: Provider,
      modelId: string,
      enabled: boolean,
      capabilities: CapabilityDeclaration[]
    ) => {
      if (!ctx.canManage() || ctx.pending() || provider.id !== ctx.providerId())
        return false;
      return ctx.run(
        () => setProviderModel(provider, modelId, enabled, capabilities),
        (_saved, isCurrent) =>
          ctx.observe(
            activation(
              provider.id,
              'Capability review saved with declared provenance.'
            ),
            isCurrent
          ),
        { key: `model-${modelId}` }
      );
    },

    certifyModel: async (
      provider: Provider,
      modelId: string,
      accept: (result: CapabilityCertification) => void
    ) => {
      if (!ctx.canManage() || ctx.pending() || provider.id !== ctx.providerId())
        return false;
      return ctx.run(
        async (isCurrent) => {
          const probe = certificationPrerequisiteReady(provider)
            ? undefined
            : (await probeProvider(provider)).result;
          if (!isCurrent()) return undefined;
          if (probe && !probe.succeeded) throw new Error(probe.detail);
          const tested = probe
            ? {
                ...provider,
                last_probe_at: probe.checked_at,
                last_probe_status: 'succeeded',
                last_probe_detail: probe.detail
              }
            : provider;
          return (await certifyProviderModel(tested, modelId)).result;
        },
        async (result, isCurrent) => {
          if (!result) return;
          await ctx.observe(
            activation(
              provider.id,
              `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`
            ),
            isCurrent
          );
          if (isCurrent()) accept(result);
        },
        { key: `certify-${modelId}` }
      );
    },

    acceptCatalog: async (digest: string, models: string[]) => {
      const current = readyForWrite();
      if (!current || !models.length) return false;
      return ctx.run(
        async () => {
          const submission = ctx.intents.catalog.prepare(
            { providerId: current.id, digest, models },
            () =>
              [{ id: current.id, etag: current.etag }, digest, models] as const
          );
          await acceptCatalogSuggestions(
            ...submission.input,
            submission.requestId
          );
          return submission.input[2].length;
        },
        async (count, isCurrent) => {
          ctx.intents.catalog.reset();
          await ctx.observe(
            activation(
              current.id,
              `Catalog facts stored for ${count} model${count === 1 ? '' : 's'}. Certify the provider's capabilities again before activation.`
            ),
            isCurrent
          );
        },
        { key: 'catalog-accept' }
      );
    },

    stageNetworkCredential: async (secret: string) => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run(
        async () => {
          if (!nativeObject(parseNativeJSON(secret)))
            throw new Error('Enter a network credential JSON object.');
          const submission = ctx.intents.networkCreate.prepare(
            { providerId: current.id, secret },
            () => [current.id, current.etag, secret] as const
          );
          const created = await createNetworkCredential(
            ...submission.input,
            submission.requestId
          );
          return { created, previousEtag: submission.input[1] };
        },
        async ({ created, previousEtag }, isCurrent) => {
          ctx.intents.networkCreate.reset();
          ctx
            .editValues()
            .document?.set(networkCredentialPath, created.credential_id);
          ctx.touch();
          await ctx.observe(
            {
              ...activation(
                current.id,
                'Network credential stored and selected in the draft.'
              ),
              snapshot: { previousEtag, etag: created.etag }
            },
            isCurrent
          );
        },
        { key: 'network-credential' }
      );
    },

    revokeNetworkCredential: async (credentialId: string) => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run(
        async () => {
          const submission = ctx.intents.networkRevoke.prepare(
            { providerId: current.id, credentialId },
            () => [current.id, current.etag, credentialId] as const
          );
          const revoked = await revokeNetworkCredential(
            ...submission.input,
            submission.requestId
          );
          return { revoked, previousEtag: submission.input[1] };
        },
        async ({ revoked, previousEtag }, isCurrent) => {
          ctx.intents.networkRevoke.reset();
          await ctx.observe(
            {
              ...activation(current.id, 'Network credential revoked.'),
              snapshot: { previousEtag, etag: revoked.etag }
            },
            isCurrent
          );
        },
        { key: 'revoke-network-credential' }
      );
    }
  };
}
