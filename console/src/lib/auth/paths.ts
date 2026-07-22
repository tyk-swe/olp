export function validatedRelativeDestination(
  candidate: string | null | undefined,
  origin: string,
  fallback = '/'
): string {
  if (!candidate || !candidate.startsWith('/') || candidate.startsWith('//') || candidate.includes('\\')) {
    return fallback;
  }
  try {
    const parsed = new URL(candidate, origin);
    if (parsed.origin !== origin) return fallback;
    return `${parsed.pathname}${parsed.search}${parsed.hash}`;
  } catch {
    return fallback;
  }
}

export function currentRelativeDestination(url: URL): string {
  return `${url.pathname}${url.search}${url.hash}`;
}
