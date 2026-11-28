import { createQuery } from '@tanstack/svelte-query';
import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
import { captureResult } from '#lib/editing/result.js';
import { errorMessage } from '#lib/management/http.js';
import { stringifyNativeJSON } from '#lib/nativeJson.js';
import {
  getProvider,
  type Provider,
  type ProviderKindCapability
} from '#lib/domain/providers/api.js';
import { providerQueries } from '#lib/domain/providers/queries.js';
import { networkCredentialPath } from '../configurationDraft.svelte';
import {
  buildCreateProviderInput,
  createProviderDraft,
  type ProviderDraft
} from '../providerEditor';
import { requiresCredential, requiresGrant } from '../providerKindSpec';
import type { WizardOperation } from './operation.svelte';

export type ConnectionDraft = ReturnType<typeof useConnectionDraft>;

function copyOf(source: Provider, spec: ProviderKindCapability) {
  const draft = createProviderDraft(spec, source.configuration);
  draft.name = `${source.name} copy`;
  draft.credential = '';
  const removed = draft.document?.at(networkCredentialPath) !== undefined;
  if (removed) draft.document?.set(networkCredentialPath, undefined);
  return { draft, removed };
}

export function useConnectionDraft(
  operation: WizardOperation,
  copyFrom: string | null,
  nothingSaved: () => boolean
) {
  const kinds = createQuery(providerQueries.kinds);
  let draft = $state<ProviderDraft | null>(null);
  const spec = $derived(
    kinds.data?.find((candidate) => candidate.kind === draft?.kind)
  );
  const grantRequired = $derived(
    Boolean(draft && spec && requiresGrant(spec, draft.authMode))
  );
  const capture = usePrincipalLifetime(() => {
    if (!draft) return;
    draft.credential = '';
    draft.externalCredentialReference = '';
  });
  function loadCopy(id: string, initial: ProviderDraft) {
    const isCurrent = capture();
    const typed = () =>
      captureResult(
        () =>
          spec && stringifyNativeJSON(buildCreateProviderInput(initial, spec))
      );
    const before = typed();
    const untouched = () => {
      const now = typed();
      return (
        isCurrent() &&
        nothingSaved() &&
        draft === initial &&
        before.ok &&
        now.ok &&
        now.value !== undefined &&
        now.value === before.value
      );
    };
    void getProvider(id)
      .then((source) => {
        if (!untouched()) return;
        const found = kinds.data?.find(
          (kind) => kind.kind === source.configuration.kind
        );
        if (!found) return;
        const copy = copyOf(source, found);
        if (copy.removed)
          operation.announce(
            'The copied connection needs its own network credential. The source credential reference was removed.'
          );
        draft = copy.draft;
      })
      .catch((error) => {
        if (untouched()) operation.report(errorMessage(error));
      });
  }
  let copyRequested = false;
  $effect(() => {
    const first = kinds.data?.[0];
    if (!draft && first) draft = createProviderDraft(first);
    if (!draft || !spec) return;
    if (!requiresCredential(spec, draft.authMode)) draft.credential = '';
  });
  $effect(() => {
    if (!copyFrom || copyRequested || !draft || !spec) return;
    copyRequested = true;
    loadCopy(copyFrom, draft);
  });
  return {
    kinds,
    get draft() {
      return draft;
    },
    set draft(next: ProviderDraft | null) {
      draft = next;
    },
    get spec() {
      return spec;
    },
    get grantRequired() {
      return grantRequired;
    },
    // Discovery records upstream model facts in the stored configuration.
    // Saving the pre-discovery document again would erase them and reset the
    // certification evidence, so going back edits the stored one.
    restore(provider: Provider) {
      if (!spec || !draft) return;
      const stored = createProviderDraft(spec, provider.configuration);
      stored.name = provider.name;
      stored.projectId = draft.projectId;
      stored.model = draft.model;
      stored.credential = '';
      draft = stored;
    }
  };
}
