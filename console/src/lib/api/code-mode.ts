import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ensureOk, unwrap, unwrapPage } from '$lib/api/http';

type Schemas = components['schemas'];
export type CodeAccount = Schemas['CodeAccount'];
export type CodeAccountWrite = Schemas['CodeAccountWrite'];
export type CodePool = Schemas['CodePool'];
export type CodePoolWrite = Schemas['CodePoolWrite'];
export type CodeRoute = Schemas['CodeRoute'];
export type CodeRouteWrite = Schemas['CodeRouteWrite'];
export type CodeBudget = Schemas['CodeBudget'];
export type CodeBudgetWrite = Schemas['CodeBudgetWrite'];
export type CodeBinding = Schemas['CodeBinding'];
export type CodeAttempt = Schemas['CodeAttempt'];
export type CodeClientConfiguration = Schemas['CodeClientConfiguration'];

export type CodeClient = CodeClientConfiguration['client'];

/** The client and models a client configuration is generated for; omitted
 * fields take the route adapter's defaults. */
export type CodeClientSelection = {
  client?: CodeClient;
  model?: string;
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

export type CodeFilters = {
  end_user_digest?: string;
  project_id?: string;
  route_id?: string;
  api_key_id?: string;
  account_id?: string;
  binding_id?: string;
  cursor?: string;
  limit?: number;
};

export async function listCodeAccounts(
  filters: CodeFilters = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/accounts', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function saveCodeAccount(
  body: CodeAccountWrite,
  current?: Pick<CodeAccount, 'id' | 'etag'>
) {
  if (current)
    return unwrap(
      await apiClient.PUT('/api/v1/code/accounts/{id}', {
        params: {
          path: { id: current.id },
          header: { 'If-Match': current.etag }
        },
        body
      })
    );
  return unwrap(
    await apiClient.POST('/api/v1/code/accounts', {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
      body
    })
  );
}

export async function listCodePools(
  filters: CodeFilters = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/pools', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function saveCodePool(
  body: CodePoolWrite,
  current?: Pick<CodePool, 'id' | 'etag'>
) {
  if (current)
    return unwrap(
      await apiClient.PUT('/api/v1/code/pools/{id}', {
        params: {
          path: { id: current.id },
          header: { 'If-Match': current.etag }
        },
        body
      })
    );
  return unwrap(
    await apiClient.POST('/api/v1/code/pools', {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
      body
    })
  );
}

export async function listCodeRoutes(
  filters: CodeFilters = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/routes', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function saveCodeRoute(
  body: CodeRouteWrite,
  current?: Pick<CodeRoute, 'id' | 'etag'>
) {
  if (current)
    return unwrap(
      await apiClient.PUT('/api/v1/code/routes/{id}', {
        params: {
          path: { id: current.id },
          header: { 'If-Match': current.etag }
        },
        body
      })
    );
  return unwrap(
    await apiClient.POST('/api/v1/code/routes', {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
      body
    })
  );
}

export async function listCodeBudgets(
  filters: CodeFilters = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/budgets', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function saveCodeBudget(
  body: CodeBudgetWrite,
  current?: Pick<CodeBudget, 'id' | 'etag'>
) {
  if (current)
    return unwrap(
      await apiClient.PUT('/api/v1/code/budgets/{id}', {
        params: {
          path: { id: current.id },
          header: { 'If-Match': current.etag }
        },
        body
      })
    );
  return unwrap(
    await apiClient.POST('/api/v1/code/budgets', {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
      body
    })
  );
}

export async function listCodeBindings(
  filters: CodeFilters = {},
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
  filters: CodeFilters = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/attempts', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function listCodeRefusals(
  filters: CodeFilters = {},
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
  filters: CodeFilters = {},
  signal?: AbortSignal
) {
  return unwrapPage(
    await apiClient.GET('/api/v1/code/token-windows', {
      params: { query: { limit: 50, ...filters } },
      signal
    })
  );
}

export async function publishCodeRoute(route: Pick<CodeRoute, 'id' | 'etag'>) {
  return unwrap(
    await apiClient.POST('/api/v1/code/routes/{id}/publish', {
      params: {
        path: { id: route.id },
        header: {
          'If-Match': route.etag,
          'Idempotency-Key': crypto.randomUUID()
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

export async function retireCodeBinding(id: string) {
  ensureOk(
    await apiClient.POST('/api/v1/code/bindings/{id}/retire', {
      params: {
        path: { id },
        header: { 'Idempotency-Key': crypto.randomUUID() }
      }
    })
  );
}
