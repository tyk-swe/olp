const LOGIN_LOOP_PATHS = ['/login', '/api/v1/oidc/login', '/api/v1/oidc/callback'] as const;
const MAX_RELATIVE_RETURN_TO_BYTES = 2_048;
const CONTROL_OR_BACKSLASH = /[\\\u0000-\u001f\u007f-\u009f]/u;
const INVALID_PERCENT_ENCODING = /%(?![0-9a-fA-F]{2})/u;

function utf8Length(value: string): number {
  return new TextEncoder().encode(value).length;
}

function containsEncodedControlOrBackslash(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    if (value[index] !== '%') continue;
    const byte = Number.parseInt(value.slice(index + 1, index + 3), 16);
    if (byte === 0x5c || byte <= 0x1f || byte === 0x7f) return true;
    index += 2;
  }
  return false;
}

function normalizeDecodedPath(pathname: string): string {
  const segments: string[] = [];
  for (const segment of pathname.replace(/^\//u, '').split('/')) {
    if (segment === '.') continue;
    if (segment === '..') {
      segments.pop();
      continue;
    }
    segments.push(segment);
  }
  return `/${segments.join('/')}`;
}

function isLoginLoop(pathname: string): boolean {
  return LOGIN_LOOP_PATHS.some((path) => pathname === path || pathname.startsWith(`${path}/`));
}

/**
 * Canonicalize a same-origin absolute-path reference for post-authentication
 * navigation. Invalid, ambiguous, external, and login-loop values fail closed
 * to `/`, matching the server-side RelativeReturnTo primitive.
 */
export function relativeReturnTo(
  value: string | null | undefined,
  origin: string = globalThis.location?.origin ?? 'http://127.0.0.1'
): string {
  if (
    !value ||
    utf8Length(value) > MAX_RELATIVE_RETURN_TO_BYTES ||
    !value.startsWith('/') ||
    value.startsWith('//') ||
    CONTROL_OR_BACKSLASH.test(value) ||
    INVALID_PERCENT_ENCODING.test(value) ||
    containsEncodedControlOrBackslash(value)
  ) {
    return '/';
  }

  try {
    const decodedValue = decodeURIComponent(value);
    if (CONTROL_OR_BACKSLASH.test(decodedValue)) return '/';
    const base = new URL(origin);
    const parsed = new URL(value, base);
    if (parsed.origin !== base.origin) return '/';
    const decodedPath = decodeURIComponent(parsed.pathname);
    const normalizedDecodedPath = normalizeDecodedPath(decodedPath);
    if (
      normalizedDecodedPath.startsWith('//') ||
      CONTROL_OR_BACKSLASH.test(decodedPath) ||
      isLoginLoop(normalizedDecodedPath)
    ) {
      return '/';
    }
    const canonical = `${parsed.pathname}${parsed.search}${parsed.hash}`;
    return utf8Length(canonical) <= MAX_RELATIVE_RETURN_TO_BYTES ? canonical : '/';
  } catch {
    return '/';
  }
}
