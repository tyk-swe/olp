import type { components, paths } from '#lib/management/schema.js';
import { apiClient } from '#lib/management/client.js';
import {
  ensureOk,
  unwrap,
  unwrapPage,
  type CursorPage
} from '#lib/management/http.js';

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
export type CodeRefusal = Schemas['CodeRefusal'];
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

export type CodeSave<Body> =
  | { kind: 'create'; body: Body; requestId: string }
  | { kind: 'update'; body: Body; current: { id: string; etag: string } };

type CollectionPath =
  | '/api/v1/code/accounts'
  | '/api/v1/code/pools'
  | '/api/v1/code/routes'
  | '/api/v1/code/budgets'
  | '/api/v1/code/bindings'
  | '/api/v1/code/attempts'
  | '/api/v1/code/decisions'
  | '/api/v1/code/refusals'
  | '/api/v1/code/token-windows';

type QueryOf<P extends CollectionPath> = NonNullable<
  paths[P]['get']['parameters']['query']
>;

export type CodeAccountsQuery = QueryOf<'/api/v1/code/accounts'>;
export type CodePoolsQuery = QueryOf<'/api/v1/code/pools'>;
export type CodeRoutesQuery = QueryOf<'/api/v1/code/routes'>;
export type CodeBudgetsQuery = QueryOf<'/api/v1/code/budgets'>;
export type CodeBindingsQuery = QueryOf<'/api/v1/code/bindings'>;
export type CodeAttemptsQuery = QueryOf<'/api/v1/code/attempts'>;
export type CodeDecisionsQuery = QueryOf<'/api/v1/code/decisions'>;
export type CodeRefusalsQuery = QueryOf<'/api/v1/code/refusals'>;
export type CodeTokenWindowsQuery = QueryOf<'/api/v1/code/token-windows'>;

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

type FetchedResult<T> = { data?: T; error?: unknown; response: Response };

async function fetchPage<T, Q extends Record<string, unknown>>(
  get: (options: {
    params: { query: Q };
    signal?: AbortSignal;
  }) => Promise<FetchedResult<{ items: T[]; next_cursor?: string | null }>>,
  filters: Q,
  signal?: AbortSignal
): Promise<CursorPage<T>> {
  return unwrapPage(
    await get({
      params: { query: { limit: 50, ...filters } as Q },
      signal
    })
  );
}

async function persistResource<Body, Saved>(
  command: CodeSave<Body>,
  endpoints: {
    create: (options: {
      params: { header: { 'Idempotency-Key': string } };
      body: Body;
    }) => Promise<FetchedResult<Saved>>;
    update: (options: {
      params: {
        path: { id: string };
        header: { 'If-Match': string };
      };
      body: Body;
    }) => Promise<FetchedResult<Saved>>;
  }
): Promise<NonNullable<Saved>> {
  if (command.kind === 'update') {
    return unwrap(
      await endpoints.update({
        params: {
          path: { id: command.current.id },
          header: { 'If-Match': command.current.etag }
        },
        body: command.body
      })
    );
  }
  return unwrap(
    await endpoints.create({
      params: { header: { 'Idempotency-Key': command.requestId } },
      body: command.body
    })
  );
}

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

export function listCodeAccounts(
  filters: CodeAccountsQuery = {},
  signal?: AbortSignal
) {
  return fetchPage(
    (opts) => apiClient.GET('/api/v1/code/accounts', opts),
    filters,
    signal
  );
}

export function saveCodeAccount(command: CodeSave<CodeAccountWrite>) {
  return persistResource(command, {
    create: (opts) => apiClient.POST('/api/v1/code/accounts', opts),
    update: (opts) => apiClient.PUT('/api/v1/code/accounts/{id}', opts)
  });
}

export function listCodePools(
  filters: CodePoolsQuery = {},
  signal?: AbortSignal
) {
  return fetchPage(
    (opts) => apiClient.GET('/api/v1/code/pools', opts),
    filters,
    signal
  );
}

export function saveCodePool(command: CodeSave<CodePoolWrite>) {
  return persistResource(command, {
    create: (opts) => apiClient.POST('/api/v1/code/pools', opts),
    update: (opts) => apiClient.PUT('/api/v1/code/pools/{id}', opts)
  });
}

export function listCodeRoutes(
  filters: CodeRoutesQuery = {},
  signal?: AbortSignal
) {
  return fetchPage(
    (opts) => apiClient.GET('/api/v1/code/routes', opts),
    filters,
    signal
  );
}

export function saveCodeRoute(command: CodeSave<CodeRouteWrite>) {
  return persistResource(command, {
    create: (opts) => apiClient.POST('/api/v1/code/routes', opts),
    update: (opts) => apiClient.PUT('/api/v1/code/routes/{id}', opts)
  });
}

export function listCodeBudgets(
  filters: CodeBudgetsQuery = {},
  signal?: AbortSignal
) {
  return fetchPage(
    (opts) => apiClient.GET('/api/v1/code/budgets', opts),
    filters,
    signal
  );
}

export function saveCodeBudget(command: CodeSave<CodeBudgetWrite>) {
  return persistResource(command, {
    create: (opts) => apiClient.POST('/api/v1/code/budgets', opts),
    update: (opts) => apiClient.PUT('/api/v1/code/budgets/{id}', opts)
  });
}

export function listCodeBindings(
  filters: CodeBindingsQuery = {},
  signal?: AbortSignal
) {
  return fetchPage(
    (opts) => apiClient.GET('/api/v1/code/bindings', opts),
    filters,
    signal
  );
}

export function listCodeAttempts(
  filters: CodeAttemptsQuery = {},
  signal?: AbortSignal
) {
  return fetchPage(
    (opts) => apiClient.GET('/api/v1/code/attempts', opts),
    filters,
    signal
  );
}

export function listCodeDecisions(
  filters: CodeDecisionsQuery = {},
  signal?: AbortSignal
) {
  return fetchPage(
    (opts) => apiClient.GET('/api/v1/code/decisions', opts),
    filters,
    signal
  );
}

export function listCodeRefusals(
  filters: CodeRefusalsQuery = {},
  signal?: AbortSignal
) {
  return fetchPage(
    (opts) => apiClient.GET('/api/v1/code/refusals', opts),
    filters,
    signal
  );
}

export function listCodeTokenWindows(
  filters: CodeTokenWindowsQuery = {},
  signal?: AbortSignal
) {
  return fetchPage(
    (opts) => apiClient.GET('/api/v1/code/token-windows', opts),
    filters,
    signal
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
