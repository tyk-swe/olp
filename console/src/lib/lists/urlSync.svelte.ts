import { goto } from '$app/navigation';
import { resolve } from '$app/paths';
import { page } from '$app/state';
import type { Pathname } from '$app/types';

/**
 * How a list page maps between its URL and its in-memory state. The URL is
 * the authority for applied filters so links are shareable; `listState` keeps
 * the draft form and cursor history between navigations.
 */
export type ListUrlSpec<State extends object> = {
  path: Pathname;
  /** The state a URL describes. */
  state: (search: URLSearchParams) => State;
  /** The canonical search string for a URL, or null when it is invalid. */
  canonical: (search: URLSearchParams) => string | null;
};

/**
 * Keeps `listState` in step with the current URL: rewrites a valid but
 * non-canonical URL in place, and resets the state whenever the search string
 * changes. `detail` names the detail-route suffix to preserve when rewriting.
 */
export function syncListWithUrl<State extends object>(
  listState: State,
  spec: ListUrlSpec<State>,
  options: { detail?: () => string; onChange?: () => void } = {}
) {
  let previousSearch = page.url.search;
  $effect(() => {
    const search = page.url.search;
    const canonical = spec.canonical(page.url.searchParams);
    if (canonical !== null) {
      const suffix = canonical ? `?${canonical}` : '';
      if (search !== suffix) {
        void goto(
          resolve(
            `${spec.path}${options.detail?.() ?? ''}${suffix}` as Pathname
          ),
          { replaceState: true, keepFocus: true, noScroll: true }
        );
      }
    }
    if (search !== previousSearch) {
      previousSearch = search;
      Object.assign(listState, spec.state(page.url.searchParams));
      options.onChange?.();
    }
  });
}

/**
 * Applies a search string: navigates when the URL changes, otherwise resets
 * the state directly because no navigation will do it.
 */
export function applyListSearch<State extends object>(
  listState: State,
  spec: ListUrlSpec<State>,
  search: string
) {
  if (search === page.url.searchParams.toString()) {
    Object.assign(listState, spec.state(page.url.searchParams));
  } else {
    void goto(
      resolve(`${spec.path}${search ? `?${search}` : ''}` as Pathname),
      {
        keepFocus: true,
        noScroll: true
      }
    );
  }
}
