import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ApiProblem, ensureSuccess, fieldIssues, result } from '$lib/api/http';

export type Plugin = components['schemas']['Plugin'];
export type PluginList = components['schemas']['PluginListResponse'];
export type PluginProfile = components['schemas']['PluginProfile'];
export type UnconfinedExecutable =
  components['schemas']['UnconfinedExecutable'];
export type UnconfinedExecutableReview =
  components['schemas']['UnconfinedExecutableReview'];

/** Installed plugins, and whether the deployment enables unconfined ones. */
export async function listPlugins(signal?: AbortSignal): Promise<PluginList> {
  const { data, error, response } = await apiClient.GET('/api/v1/plugins', {
    signal
  });
  return result(data, error, response);
}

/**
 * Uploads a plugin module. `created` is false when the digest was already
 * installed, in which case nothing changed.
 */
export async function installPlugin(
  module: Blob
): Promise<{ plugin: Plugin; created: boolean }> {
  const { data, error, response } = await apiClient.POST('/api/v1/plugins', {
    // The contract types a binary body as a string; the module is sent as its
    // raw bytes rather than through the client's JSON serializer.
    body: module as unknown as string,
    bodySerializer: (body: unknown) => body,
    headers: { 'Content-Type': 'application/wasm' }
  });
  return {
    plugin: result(data, error, response),
    created: response.status === 201
  };
}

/** Approves exactly the origins the plugin declares. */
export async function approvePlugin(plugin: Plugin): Promise<Plugin> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/plugins/{plugin_digest}/approve',
    {
      params: {
        path: { plugin_digest: plugin.digest },
        header: { 'If-Match': plugin.etag }
      },
      body: { origins: plugin.manifest.origins }
    }
  );
  return result(data, error, response);
}

export async function uninstallPlugin(plugin: Plugin): Promise<void> {
  const { error, response } = await apiClient.DELETE(
    '/api/v1/plugins/{plugin_digest}',
    {
      params: {
        path: { plugin_digest: plugin.digest },
        header: { 'If-Match': plugin.etag }
      }
    }
  );
  ensureSuccess(error, response);
}

/** The executables in the deployment's unconfined plugin directory. */
export async function listUnconfinedExecutables(
  signal?: AbortSignal
): Promise<UnconfinedExecutable[]> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/unconfined-plugins',
    { signal }
  );
  return result(data, error, response).items;
}

/** Runs an executable to read the manifest it declares, for review. */
export async function reviewUnconfinedExecutable(
  name: string
): Promise<UnconfinedExecutableReview> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/unconfined-plugins/{executable}/review',
    { params: { path: { executable: name } } }
  );
  return result(data, error, response);
}

/**
 * Permits the reviewed build of an executable as an unconfined plugin. The
 * owner acknowledged the risk and reauthenticated for plugin_permit.
 */
export async function permitUnconfinedPlugin(
  review: UnconfinedExecutableReview
): Promise<Plugin> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/unconfined-plugins/{executable}/permit',
    {
      params: { path: { executable: review.name } },
      body: { digest: review.digest, acknowledge_risk: true }
    }
  );
  return result(data, error, response);
}

/** Whether a request needs the owner to reauthenticate first. */
export function needsReauthentication(error: unknown): boolean {
  return error instanceof ApiProblem && error.problem.status === 428;
}

/**
 * Explains why OLP refused a plugin, naming the manifest field a typed
 * refusal points at.
 */
export function pluginProblem(error: unknown): string | null {
  if (!(error instanceof ApiProblem)) return null;
  const field = fieldIssues(error)[0]?.field;
  return field ? `${error.message} (${field})` : error.message;
}

/** The first twelve digest characters, enough to tell installs apart. */
export function shortDigest(digest: string): string {
  return digest.slice(0, 12);
}
