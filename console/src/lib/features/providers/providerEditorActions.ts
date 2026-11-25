import type { QueryClient } from '@tanstack/svelte-query';
import { errorMessage } from '#lib/management/http.js';
import { nativeObject, parseNativeJSON } from '#lib/nativeJson.js';
import type { MutationIntentGroup } from '#lib/editing/editorSession.svelte.js';
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
  type CapabilityDeclaration,
  type ProviderKindCapability,
  type ProviderModelPage
} from './models';
import {
  revokeProviderCredential,
  rotateProviderCredential,
  type ExternalCredentialReference,
  type ProviderCredential
} from './credentials';
import { diffProviderRevisions, restoreProviderRevision } from './revisions';
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
  readonly run: <T>(
    label: string,
    action: (isCurrent: () => boolean) => Promise<T>
  ) => Promise<T | undefined>;
  readonly observe: (
    receipt: ProviderChangeReceipt,
    isCurrent: () => boolean
  ) => Promise<void>;
};

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
      return ctx.run('save', async (isCurrent) => {
        const snapshotEtag = ctx.snapshotEtag();
        if (!snapshotEtag)
          throw new Error('Reload the provider before saving.');
        const version = ctx.editVersion();
        const updated = await updateProvider(
          current.id,
          snapshotEtag,
          buildUpdateProviderInput(ctx.editValues(), spec)
        );
        if (!isCurrent()) return;
        const stillDirty = ctx.editVersion() !== version;
        ctx.markDraftSaved(updated.etag, stillDirty);
        ctx.intents.reset('activate', 'disable', 'restore');
        ctx.queryClient.setQueriesData<ProviderModelPage>(
          { queryKey: providerKeys.modelsOf(current.id) },
          (page) => (page ? { ...page, provider: updated } : page)
        );
        ctx.resetModelPage();
        await ctx.observe(
          {
            change: { kind: 'activation', id: current.id },
            notice: stillDirty
              ? 'Draft saved. You have additional unsaved changes.'
              : 'Provider draft settings saved.'
          },
          isCurrent
        );
      });
    },

    probe: async () => {
      const current = readyForWrite();
      if (!current || ctx.dirty()) return false;
      return ctx.run('detail-probe', async (isCurrent) => {
        const { result } = await probeProvider(current);
        if (!isCurrent()) return;
        if (!result.succeeded) throw new Error(result.detail);
        await ctx.observe(
          {
            change: { kind: 'activation', id: current.id },
            notice: `Connection succeeded: ${probeSummary(result)}`
          },
          isCurrent
        );
      });
    },

    activate: async () => {
      const current = readyForWrite();
      if (!current || ctx.dirty()) return false;
      return ctx.run('detail-activate', async (isCurrent) => {
        const submission = ctx.intents.activate.prepare(current.id, () => ({
          id: current.id,
          etag: current.etag
        }));
        const generation = await activateProvider(
          submission.input,
          submission.requestId
        );
        if (!isCurrent()) return;
        ctx.intents.activate.reset();
        await ctx.observe(
          {
            change: { kind: 'activation', id: current.id },
            notice: `Activated in runtime generation ${generation}.`
          },
          isCurrent
        );
      });
    },

    disable: async () => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run('detail-disable', async (isCurrent) => {
        const submission = ctx.intents.disable.prepare(current.id, () => ({
          id: current.id,
          etag: current.etag
        }));
        try {
          const generation = await disableProvider(
            submission.input,
            submission.requestId
          );
          if (!isCurrent()) return;
          ctx.intents.disable.reset();
          await ctx.observe(
            {
              change: { kind: 'activation', id: current.id },
              notice: disableNotice(generation)
            },
            isCurrent
          );
        } catch (error) {
          if (!isCurrent()) return;
          if (!isProviderInUse(error)) throw error;
          ctx.setReferenceConflict(errorMessage(error));
        }
      });
    },

    restoreDraft: async () => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run('detail-restore-draft', async (isCurrent) => {
        const submission = ctx.intents.restore.prepare(current.id, () => ({
          id: current.id,
          etag: current.etag
        }));
        await restoreProviderAsDraft(submission.input, submission.requestId);
        if (!isCurrent()) return;
        ctx.intents.restore.reset();
        await ctx.observe(
          {
            change: { kind: 'activation', id: current.id },
            notice:
              'Provider restored as a draft. Stored capabilities are declared again: re-certify and test before activation.'
          },
          isCurrent
        );
      });
    },

    stageCredential: async (
      credential: string | ExternalCredentialReference
    ) => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run('rotate-credential', async (isCurrent) => {
        const submission = ctx.intents.stage.prepare(
          { providerId: current.id, credential },
          () => [{ id: current.id, etag: current.etag }, credential] as const
        );
        await rotateProviderCredential(
          ...submission.input,
          submission.requestId
        );
        if (!isCurrent()) return;
        ctx.intents.stage.reset();
        ctx.resetModelPage();
        await ctx.observe(
          {
            change: { kind: 'activation', id: current.id },
            notice:
              'Credential version staged. Test and activate the provider to publish it; the current runtime credential remains live until then.'
          },
          isCurrent
        );
        return true;
      });
    },

    revokeCredential: async (credential: ProviderCredential) => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run(`revoke-${credential.id}`, async (isCurrent) => {
        const submission = ctx.intents.revoke.prepare(
          { providerId: current.id, credentialId: credential.id },
          () => [{ id: current.id, etag: current.etag }, credential.id] as const
        );
        await revokeProviderCredential(
          ...submission.input,
          submission.requestId
        );
        if (!isCurrent()) return;
        ctx.intents.revoke.reset();
        await ctx.observe(
          {
            change: { kind: 'activation', id: current.id },
            notice: `Credential version ${credential.version} revoked.`
          },
          isCurrent
        );
      });
    },

    restoreRevision: async (revisionId: string, revision: number) => {
      const current = readyForWrite();
      if (!current) return false;
      return ctx.run('revision-restore', async (isCurrent) => {
        const submission = ctx.intents.revision.prepare(
          { providerId: current.id, revisionId },
          () => ({
            provider: { id: current.id, etag: current.etag },
            revisionId
          })
        );
        await restoreProviderRevision(
          submission.input.provider,
          submission.input.revisionId,
          submission.requestId
        );
        if (!isCurrent()) return;
        ctx.intents.revision.reset();
        ctx.resetModelPage();
        await ctx.observe(
          {
            change: { kind: 'activation', id: current.id },
            notice: `Revision ${revision} restored as a new draft. No historical credential was restored; the current credential selection was preserved. Test and certify before activation.`
          },
          isCurrent
        );
        return true;
      });
    },

    compareListing: async () => {
      const current = readyForWrite();
      if (!current) return;
      return ctx.run('listing-comparison', () =>
        compareProviderListing(current.id)
      );
    },

    compareRevisions: async (from: string, to: string) => {
      const current = ctx.current();
      if (!current) return;
      return ctx.run('revision-diff', () =>
        diffProviderRevisions(current.id, from, to)
      );
    },

    discoverModels: async () => {
      const current = readyForWrite();
      if (!current) return;
      return ctx.run('detail-discover', async (isCurrent) => {
        const updated = await discoverProviderModels(current);
        if (!isCurrent()) return;
        ctx.resetModelPage();
        await ctx.observe(
          {
            change: { kind: 'activation', id: current.id },
            notice: `${updated.model_count} model${updated.model_count === 1 ? '' : 's'} discovered. Review capabilities before activation.`
          },
          isCurrent
        );
        return true;
      });
    },

    declareModels: async (input: string) => {
      const current = readyForWrite();
      if (!current) return;
      const names = parseManualModelNames(input);
      if (!names.length) {
        ctx.reportError('Enter at least one upstream model identifier.');
        return;
      }
      return ctx.run('detail-declare', async (isCurrent) => {
        const updated = await declareProviderModels(current, names);
        if (!isCurrent()) return;
        ctx.resetModelPage();
        await ctx.observe(
          {
            change: { kind: 'activation', id: current.id },
            notice: `${updated.model_count} manually declared model${updated.model_count === 1 ? '' : 's'} ready for capability review.`
          },
          isCurrent
        );
        return true;
      });
    },

    reviewModel: async (
      provider: Provider,
      modelId: string,
      enabled: boolean,
      capabilities: CapabilityDeclaration[]
    ) => {
      if (!ctx.canManage() || ctx.pending() || provider.id !== ctx.providerId())
        return false;
      return ctx.run(`model-${modelId}`, async (isCurrent) => {
        await setProviderModel(provider, modelId, enabled, capabilities);
        if (!isCurrent()) return false;
        await ctx.observe(
          {
            change: { kind: 'activation', id: provider.id },
            notice: 'Capability review saved with declared provenance.'
          },
          isCurrent
        );
        return true;
      });
    },

    certifyModel: async (provider: Provider, modelId: string) => {
      if (!ctx.canManage() || ctx.pending() || provider.id !== ctx.providerId())
        return;
      return ctx.run(`certify-${modelId}`, async (isCurrent) => {
        const probe = certificationPrerequisiteReady(provider)
          ? undefined
          : (await probeProvider(provider)).result;
        if (!isCurrent()) return;
        if (probe && !probe.succeeded) throw new Error(probe.detail);
        const tested = probe
          ? {
              ...provider,
              last_probe_at: probe.checked_at,
              last_probe_status: 'succeeded',
              last_probe_detail: probe.detail
            }
          : provider;
        const { result } = await certifyProviderModel(tested, modelId);
        if (!isCurrent()) return;
        await ctx.observe(
          {
            change: { kind: 'activation', id: provider.id },
            notice: `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`
          },
          isCurrent
        );
        return result;
      });
    },

    acceptCatalog: async (digest: string, models: string[]) => {
      const current = readyForWrite();
      if (!current || !models.length) return;
      return ctx.run('catalog-accept', async (isCurrent) => {
        const submission = ctx.intents.catalog.prepare(
          { providerId: current.id, digest, models },
          () =>
            [{ id: current.id, etag: current.etag }, digest, models] as const
        );
        await acceptCatalogSuggestions(
          ...submission.input,
          submission.requestId
        );
        if (!isCurrent()) return;
        ctx.intents.catalog.reset();
        const count = submission.input[2].length;
        await ctx.observe(
          {
            change: { kind: 'activation', id: current.id },
            notice: `Catalog facts stored for ${count} model${count === 1 ? '' : 's'}. Certify the provider's capabilities again before activation.`
          },
          isCurrent
        );
        return true;
      });
    },

    stageNetworkCredential: async (secret: string) => {
      const current = readyForWrite();
      if (!current) return;
      return ctx.run('network-credential', async (isCurrent) => {
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
        if (!isCurrent()) return;
        ctx.intents.networkCreate.reset();
        ctx
          .editValues()
          .document?.set(networkCredentialPath, created.credential_id);
        ctx.touch();
        await ctx.observe(
          {
            change: { kind: 'activation', id: current.id },
            snapshot: { previousEtag: submission.input[1], etag: created.etag },
            notice: 'Network credential stored and selected in the draft.'
          },
          isCurrent
        );
        return true;
      });
    },

    revokeNetworkCredential: async (credentialId: string) => {
      const current = readyForWrite();
      if (!current) return;
      return ctx.run('revoke-network-credential', async (isCurrent) => {
        const submission = ctx.intents.networkRevoke.prepare(
          { providerId: current.id, credentialId },
          () => [current.id, current.etag, credentialId] as const
        );
        const revoked = await revokeNetworkCredential(
          ...submission.input,
          submission.requestId
        );
        if (!isCurrent()) return;
        ctx.intents.networkRevoke.reset();
        await ctx.observe(
          {
            change: { kind: 'activation', id: current.id },
            snapshot: { previousEtag: submission.input[1], etag: revoked.etag },
            notice: 'Network credential revoked.'
          },
          isCurrent
        );
      });
    }
  };
}
