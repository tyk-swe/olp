/**
 * Writes `value` to the system clipboard, reporting failure instead of
 * throwing. The async Clipboard API rejects in insecure contexts, in browsers
 * that do not implement it, and whenever the user or a permission policy
 * denies the write — so every caller has to offer a manual fallback.
 */
export async function copyText(value: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(value);
    return true;
  } catch {
    return false;
  }
}
