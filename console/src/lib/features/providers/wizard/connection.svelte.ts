import { createQuery, useQueryClient } from '@tanstack/svelte-query';
import type { QueryClient } from '@tanstack/svelte-query';
import { useMutationIntentGroup } from '#lib/editing/editorSession.svelte.js';
import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
import type {
  Provider,
  ProviderKindCapability,
  ProviderVendor
} from '#lib/domain/providers/api.js';
import { providerKeys } from '#lib/domain/providers/providerKeys.js';
import { providerQueries } from '#lib/domain/providers/queries.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import { GrantEnrollmentState } from '../grantEnrollment.svelte';
import { refreshProviderReceipt } from '../providerChanges';
import type { ProviderDraft } from '../providerEditor';
import type { ProviderProfile } from '../providerProfiles';
import {
  isLiveGrant,
  resolveConnectionProgress,
  saveProviderConnection,
  type ConnectionProgress
} from './connectionProgress';
import type { ConnectionDraft } from './connectionDraft.svelte';
import {
  connectionServerFields,
  validateConnection,
  type ConnectionField
} from './connectionValidation';
import type { WizardOperation } from './operation.svelte';
import type { SavedProvider } from './savedProvider.svelte';

type Dependencies = {
  operation: WizardOperation;
  saved: SavedProvider;
  form: ConnectionDraft;
  canManage: () => boolean;
  retireActivation: () => void;
  showStep: (step: 2) => void;
};

async function holdsLiveGrant(client: QueryClient, provider: Provider) {
  if (!provider.draft_credential_id) return false;
  const credentials = await client.fetchQuery({
    ...providerQueries.credentials(provider.id),
    staleTime: 0
  });
  return isLiveGrant(
    credentials,
    provider.draft_credential_id,
    provider.configuration.profile_revision
  );
}

function validateSubmission(
  client: QueryClient,
  draft: ProviderDraft,
  spec: ProviderKindCapability,
  credentialAlreadyStored: boolean
) {
  return validateConnection(draft, spec, {
    credentialAlreadyStored,
    profiles: client.getQueryData<ProviderProfile[]>(providerKeys.profiles()),
    vendors: client.getQueryData<ProviderVendor[]>(providerKeys.vendors())
  });
}

type TestSaved = (
  provider: Provider,
  isCurrent: () => boolean
) => Promise<void>;

function useWizardGrant(
  { operation, saved, form, canManage }: Dependencies,
  test: TestSaved
) {
  const queryClient = useQueryClient();
  const access = useRole();
  const principal = access.user?.id;
  const enrollment = new GrantEnrollmentState({
    allowed: () => canManage() && access.user?.id === principal,
    error: (message, cause) => operation.report(message, cause),
    completed: async (_completion, { isCurrent }) => {
      operation.announce(
        'Grant enrolled. Testing the connection remains a separate step.'
      );
      await queryClient.invalidateQueries(
        { queryKey: providerKeys.credentials(saved.id) },
        { throwOnError: true }
      );
      if (!isCurrent()) return;
      const { provider } = await saved.refetch();
      if (isCurrent()) await test(provider, isCurrent);
    }
  });
  const draftCredentials = createQuery(() => ({
    ...providerQueries.credentials(saved.id),
    enabled: form.grantRequired && Boolean(saved.provider?.draft_credential_id)
  }));
  return {
    enrollment,
    get enrolled() {
      return isLiveGrant(
        draftCredentials.data,
        saved.provider?.draft_credential_id,
        form.draft?.profileRevision
      );
    }
  };
}

export function useWizardConnection(dependencies: Dependencies) {
  const { operation, saved, form, canManage, showStep, retireActivation } =
    dependencies;
  const queryClient = useQueryClient();
  const intents = useMutationIntentGroup(['create', 'rotateCredential']);
  let progress = $state.raw<ConnectionProgress | undefined>();
  const grant = useWizardGrant(dependencies, test);
  usePrincipalLifetime(() => {
    progress = undefined;
    grant.enrollment.input = '';
  });
  async function test(provider: Provider, isCurrent: () => boolean) {
    await saved.test(provider, isCurrent);
    if (isCurrent()) showStep(2);
  }
  async function signInOrTest(isCurrent: () => boolean) {
    const notice =
      'Provider saved. Connection testing remains a separate step.';
    operation.announce(notice);
    const refreshed = await refreshProviderReceipt(
      queryClient,
      { change: { kind: 'connection' }, notice },
      saved.refetch,
      isCurrent
    );
    if (refreshed.kind === 'stale') return;
    if (refreshed.kind === 'refresh-failed')
      return operation.reportRefreshFailure(refreshed.message);
    const { provider } = refreshed.value;
    const enrolled =
      !form.grantRequired || (await holdsLiveGrant(queryClient, provider));
    if (!isCurrent()) return;
    if (enrolled) await test(provider, isCurrent);
    else await grant.enrollment.start(provider);
    if (isCurrent() && (enrolled || grant.enrollment.enrollment))
      progress = undefined;
  }
  function save(
    next: ConnectionProgress,
    draft: ProviderDraft,
    serverFields: Readonly<Record<string, ConnectionField>>
  ) {
    return operation.run(
      'create',
      (isCurrent) =>
        saveProviderConnection(next, {
          createIntent: intents.create,
          rotateIntent: intents.rotateCredential,
          isCurrent,
          setProgress: (value) => {
            progress = value;
          },
          setNotice: operation.announce
        }),
      {
        serverFields,
        accept: async (submission, isCurrent) => {
          if (!submission) return;
          draft.credential = '';
          saved.adopt(submission.id);
          await signInOrTest(isCurrent);
        }
      }
    );
  }
  return {
    grant: grant.enrollment,
    get started() {
      return progress !== undefined;
    },
    get grantEnrolled() {
      return grant.enrolled;
    },
    async submit() {
      const { draft, spec } = form;
      const busy = operation.busy || grant.enrollment.busy;
      if (!canManage() || busy || !draft || !spec) return;
      const checked = validateSubmission(
        queryClient,
        draft,
        spec,
        Boolean(saved.provider || progress?.phase === 'saved')
      );
      if (!operation.check(checked)) return;
      const resolved = resolveConnectionProgress(
        checked.value,
        progress,
        saved.provider
      );
      if (!resolved.progress) return operation.report(resolved.error ?? '');
      if (!progress) retireActivation();
      progress = resolved.progress;
      await save(resolved.progress, draft, connectionServerFields(draft, spec));
    }
  };
}
