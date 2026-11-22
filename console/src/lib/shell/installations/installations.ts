import { captureResult, type Result } from '#lib/editing/result.js';

export type InstallationBookmark = { name: string; origin: string };
const storageKey = 'olp.installations.v1';

export function installationOrigin(value: string): string {
  const url = new URL(value);
  const loopback =
    url.hostname === 'localhost' ||
    url.hostname === '[::1]' ||
    /^127\.\d+\.\d+\.\d+$/.test(url.hostname);
  if (
    url.username ||
    url.password ||
    url.search ||
    url.hash ||
    url.pathname !== '/' ||
    (url.protocol !== 'https:' && !(url.protocol === 'http:' && loopback))
  ) {
    throw new Error(
      'Use an HTTPS installation origin, or HTTP on loopback, without a path or credentials.'
    );
  }
  return url.origin;
}

export function bookmarkOrigin(
  value: string,
  current: string,
  bookmarks: InstallationBookmark[]
): string {
  const target = installationOrigin(value);
  const hostname = new URL(target).hostname;
  for (const origin of [current, ...bookmarks.map((entry) => entry.origin)]) {
    if (origin !== target && new URL(origin).hostname === hostname) {
      throw new Error(
        'Use a distinct hostname for each installation. Browser cookies are shared across ports on the same hostname.'
      );
    }
  }
  return target;
}

export function readInstallations(
  storage: Pick<Storage, 'getItem'>
): InstallationBookmark[] {
  try {
    const value: unknown = JSON.parse(storage.getItem(storageKey) || '[]');
    if (!Array.isArray(value)) return [];
    const seen = new Set<string>();
    return value.slice(0, 20).flatMap((entry) => {
      if (
        !entry ||
        typeof entry.name !== 'string' ||
        typeof entry.origin !== 'string'
      )
        return [];
      const name = entry.name.trim();
      const origin = installationOrigin(entry.origin);
      if (!name || name.length > 100 || seen.has(origin)) return [];
      seen.add(origin);
      return [{ name, origin }];
    });
  } catch {
    return [];
  }
}

export function saveInstallations(
  storage: Pick<Storage, 'setItem'>,
  bookmarks: InstallationBookmark[]
) {
  storage.setItem(storageKey, JSON.stringify(bookmarks));
}

export function storedBookmarks(
  storage: () => Pick<Storage, 'getItem'>,
  current: string
): Result<InstallationBookmark[]> {
  return captureResult(() => {
    const kept: InstallationBookmark[] = [];
    for (const entry of readInstallations(storage())) {
      try {
        bookmarkOrigin(entry.origin, current, kept);
        kept.push(entry);
      } catch {
        /* Ignore stale bookmarks that would share installation cookies. */
      }
    }
    return kept;
  });
}

export function addBookmark(
  storage: () => Pick<Storage, 'setItem'>,
  current: string,
  bookmarks: InstallationBookmark[],
  name: string,
  url: string
): Result<InstallationBookmark[]> {
  return captureResult(() => {
    const target = bookmarkOrigin(url, current, bookmarks);
    const label = name.trim();
    if (!label || label.length > 100)
      throw new Error('Use a name of 1–100 characters.');
    const next = [
      ...bookmarks.filter((entry) => entry.origin !== target),
      { name: label, origin: target }
    ];
    if (next.length > 20)
      throw new Error('Keep at most 20 installation bookmarks.');
    saveInstallations(storage(), next);
    return next;
  });
}

export function removeBookmark(
  storage: () => Pick<Storage, 'setItem'>,
  bookmarks: InstallationBookmark[],
  target: string
): Result<InstallationBookmark[]> {
  return captureResult(() => {
    const next = bookmarks.filter((entry) => entry.origin !== target);
    saveInstallations(storage(), next);
    return next;
  });
}
