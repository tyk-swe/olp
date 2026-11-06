import type { components, paths } from '$lib/management/schema';
import { apiClient } from '$lib/management/client';
import { ensureOk, unwrap, unwrapPage } from '$lib/management/http';

type Schemas = components['schemas'];
export type CodeAccount = Schemas['CodeAccount'];
export type CodeAccountCooldown = Schemas['CodeAccountCooldown'];
export type CodeAllowanceWindow = Schemas['CodeAllowanceWindow'];
export type CodeAccountWrite = Schemas['CodeAccountWrite'];
export type CodePool = Schemas['CodePool'];
export type CodePoolWrite = Schemas['CodePoolWrite'];
export type CodeRoute = Schemas['CodeRoute'];
export type CodeRouteWrite = Schemas['CodeRouteWrite'];
export type CodeBudget = Schemas['CodeBudget'];
export type CodeBudgetWrite = Schemas['CodeBudgetWrite'];
export type CodeBinding = Schemas['CodeBinding'];
export type CodeAttempt = Schemas['CodeAttempt'];
export type CodeDecision = Schemas['CodeDecision'];
export type CodeClientConfiguration = Schemas['CodeClientConfiguration'];

export type CodeClient = CodeClientConfiguration['client'];
export type CodeClientPlatform = CodeClientConfiguration['platform'];

export type CodeClientSelection = {
  client?: CodeClient;
  platform?: CodeClientPlatform;
  model?: string;
  plan_model?: string;
  small_model?: string;
};

export async function getCodeClientConfiguration(
  route: CodeRoute,
  gatewayURL: string,
  selection: CodeClientSelection = {},
  signal?: AbortSignal
): Promise<CodeClientConfiguration> {
  return unwrap(
    await apiClient.GET('/api/v1/code/routes/{id}/client-config', {
      params: {
        path: { id: route.id },
        query: { gateway_url: gatewayURL, ...selection }
      },
      signal
    })
  );
}

export type CodeSave<Body> =
  | { kind: 'create'; body: Body; requestId: string }
  | { kind: 'update'; body: Body; current: { id: string; etag: string } };

export type CodeAccountsQuery = NonNullable<
  paths['/api/v1/code/accounts']['get']['parameters']['query']
>;
export type CodePoolsQuery = NonNullable<
  paths['/api/v1/code/pools']['get']['parameters']['query']
>;
export type CodeRoutesQuery = NonNullable<
  paths['/api/v1/code/routes']['get']['parameters']['query']
>;
export type CodeBudgetsQuery = NonNullable<
  paths['/api/v1/code/budgets']['get']['parameters']['query']
>;
export type CodeBindingsQuery = NonNullable<
  paths['/api/v1/code/bindings']['get']['parameters']['query']
>;
export type CodeAttemptsQuery = NonNullable<
  paths['/api/v1/code/attempts']['get']['parameters']['query']
>;
export type CodeDecisionsQuery = NonNullable<
  paths['/api/v1/code/decisions']['get']['parameters']['query']
>;
export type CodeRefusalsQuery = NonNullable<
  paths['/api/v1/code/refusals']['get']['parameters']['query']
>;
export type CodeTokenWindowsQuery = NonNullable<
  paths['/api/v1/code/token-windows']['get']['parameters']['query']
>;

export type CodeFilters =
  | CodeAccountsQuery
  | CodePoolsQuery
  | CodeRoutesQuery
  | CodeBudgetsQuery
  | CodeBindingsQuery
  | CodeAttemptsQuery
  | CodeDecisionsQuery
  | CodeRefusalsQuery
  | CodeTokenWindowsQuery;

export async function listCodeAccounts(
  filters: CodeAccountsQuery = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/accounts', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function saveCodeAccount(command: CodeSave<CodeAccountWrite>) {
  if (command.kind === 'update')
    return unwrap(
      await apiClient.PUT('/api/v1/code/accounts/{id}', {
        params: {
          path: { id: command.current.id },
          header: { 'If-Match': command.current.etag }
        },
        body: command.body
      })
    );
  return unwrap(
    await apiClient.POST('/api/v1/code/accounts', {
      params: { header: { 'Idempotency-Key': command.requestId } },
      body: command.body
    })
  );
}

export async function listCodePools(
  filters: CodePoolsQuery = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/pools', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function saveCodePool(command: CodeSave<CodePoolWrite>) {
  if (command.kind === 'update')
    return unwrap(
      await apiClient.PUT('/api/v1/code/pools/{id}', {
        params: {
          path: { id: command.current.id },
          header: { 'If-Match': command.current.etag }
        },
        body: command.body
      })
    );
  return unwrap(
    await apiClient.POST('/api/v1/code/pools', {
      params: { header: { 'Idempotency-Key': command.requestId } },
      body: command.body
    })
  );
}

export async function listCodeRoutes(
  filters: CodeRoutesQuery = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/routes', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function saveCodeRoute(command: CodeSave<CodeRouteWrite>) {
  if (command.kind === 'update')
    return unwrap(
      await apiClient.PUT('/api/v1/code/routes/{id}', {
        params: {
          path: { id: command.current.id },
          header: { 'If-Match': command.current.etag }
        },
        body: command.body
      })
    );
  return unwrap(
    await apiClient.POST('/api/v1/code/routes', {
      params: { header: { 'Idempotency-Key': command.requestId } },
      body: command.body
    })
  );
}

export async function listCodeBudgets(
  filters: CodeBudgetsQuery = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/budgets', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function saveCodeBudget(command: CodeSave<CodeBudgetWrite>) {
  if (command.kind === 'update')
    return unwrap(
      await apiClient.PUT('/api/v1/code/budgets/{id}', {
        params: {
          path: { id: command.current.id },
          header: { 'If-Match': command.current.etag }
        },
        body: command.body
      })
    );
  return unwrap(
    await apiClient.POST('/api/v1/code/budgets', {
      params: { header: { 'Idempotency-Key': command.requestId } },
      body: command.body
    })
  );
}

export async function listCodeBindings(
  filters: CodeBindingsQuery = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/bindings', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function listCodeAttempts(
  filters: CodeAttemptsQuery = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/attempts', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function listCodeDecisions(
  filters: CodeDecisionsQuery = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/decisions', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function listCodeRefusals(
  filters: CodeRefusalsQuery = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/refusals', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function listCodeTokenWindows(
  filters: CodeTokenWindowsQuery = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/token-windows', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function publishCodeRoute(
  route: Pick<CodeRoute, 'id' | 'etag'>,
  requestId: string
) {
  return unwrap(
    await apiClient.POST('/api/v1/code/routes/{id}/publish', {
      params: {
        path: { id: route.id },
        header: {
          'If-Match': route.etag,
          'Idempotency-Key': requestId
        }
      }
    })
  );
}

export async function listCodeRevisions(
  id: string,
  cursor?: string,
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/routes/{id}/revisions', {
      params: { path: { id }, query: { limit: 50, cursor } },
      signal
    })
  );
}

export async function requestCodeAccountAllowanceRead(
  account: Pick<CodeAccount, 'id'>,
  requestId: string
) {
  if (!requestId)
    throw new Error('An allowance read requires a stable request ID.');
  return unwrap(
    await apiClient.POST('/api/v1/code/accounts/{id}/allowance-reads', {
      params: {
        path: { id: account.id },
        header: { 'Idempotency-Key': requestId }
      }
    })
  );
}

export async function retireCodeBinding(id: string, requestId: string) {
  ensureOk(
    await apiClient.POST('/api/v1/code/bindings/{id}/retire', {
      params: {
        path: { id },
        header: { 'Idempotency-Key': requestId }
      }
    })
  );
}
