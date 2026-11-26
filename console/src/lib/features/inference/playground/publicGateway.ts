import { captureResult } from '#lib/editing/result.js';
import { parseNativeJSON, type NativeValue } from '#lib/nativeJson.js';
import { validRouteSlug } from '#lib/routeIdentity.js';

export interface OutcomeOk<T> {
  readonly ok: true;
  readonly value: T;
}

export interface OutcomeErr {
  readonly ok: false;
  readonly error: string;
}

export type Outcome<T> = OutcomeOk<T> | OutcomeErr;

export const MAX_PUBLIC_REQUEST_BYTES = 1 << 20;
export const MAX_PUBLIC_RESPONSE_BYTES = 8 << 20;

const utf8 = new TextEncoder();

export function fail(error: string): Outcome<never> {
  return { ok: false, error };
}

export function utf8ByteLength(text: string): number {
  return utf8.encode(text).byteLength;
}

export function validateApiKey(key: string): Outcome<string> {
  if (!key.startsWith('olp_') || key.length > 512)
    return fail('Enter an inference API key for this route.');
  return { ok: true, value: key };
}

export function validatePublicRoute(route: string): Outcome<string> {
  if (!validRouteSlug(route))
    return fail('Choose an active published route slug.');
  return { ok: true, value: route };
}

export function publicGatewayHeaders(
  key: string,
  extra?: Readonly<Record<string, string>>
): Headers {
  return new Headers({
    Authorization: `Bearer ${key}`,
    'Cache-Control': 'no-store',
    ...extra
  });
}

export function parseNativeValue(
  source: string,
  fallbackError: string,
  maxBytes?: number
): Outcome<NativeValue> {
  const result = captureResult(() => parseNativeJSON(source, maxBytes));
  if (!result.ok) {
    return fail(
      result.error instanceof Error ? result.error.message : fallbackError
    );
  }
  return { ok: true, value: result.value };
}

export function unwrapOutcome<T>(outcome: Outcome<T>): T {
  if (!outcome.ok) throw new Error(outcome.error);
  return outcome.value;
}
