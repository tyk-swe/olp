export function currentRelativeDestination(url: URL): string {
  return `${url.pathname}${url.search}${url.hash}`;
}
