import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ApiProblem, ensureSuccess, fieldIssues, result } from '$lib/api/http';

export type Plugin = components['schemas']['Plugin'];
export type PluginProfile = components['schemas']['PluginProfile'];

export async function listPlugins(signal?: AbortSignal): Promise<Plugin[]> {
  const { data, error, response } = await apiClient.GET('/api/v1/plugins', {
    signal
  });
  return result(data, error, response).items;
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
