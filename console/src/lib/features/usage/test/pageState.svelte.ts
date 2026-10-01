/** A reactive stand-in for `$app/state`. Like SvelteKit's, its URL is
 * replaced on navigation, never mutated in place. */
class PageState {
  url = $state.raw(new URL('https://console.test/usage'));
}

export const page = new PageState();
