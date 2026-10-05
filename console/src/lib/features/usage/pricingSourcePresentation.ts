import type {
  PricingSource,
  PricingSourceSnapshot,
  ReferenceCatalog
} from './api/pricingSources';
import { formatDate } from '$lib/format';

/** How a pricing source is read, as the console names it. */
export function sourceFormatLabel(source: Pick<PricingSource, 'format'>) {
  return source.format === 'catalog'
    ? 'Signed reference catalog'
    : 'Price list';
}

/** Where a source's document comes from. */
export function sourceAddress(source: Pick<PricingSource, 'format' | 'url'>) {
  return source.url ?? 'Catalog bundled with this release';
}

/** The signed catalog a snapshot was mapped from, or '' for a price list. */
export function catalogProvenanceLabel(
  snapshot: Pick<PricingSourceSnapshot, 'catalog'>
): string {
  const catalog = snapshot.catalog;
  if (!catalog) return '';
  return `Catalog published ${formatDate(catalog.published_at)} · signed by ${catalog.key_id} · ${catalog.sha256.slice(0, 12)}…`;
}

/** The hint for a catalog source created without a URL. */
export function bundledCatalogHint(catalog: ReferenceCatalog | undefined) {
  if (!catalog)
    return 'Leave the URL empty to read the catalog bundled with this release.';
  return `Leave the URL empty to read the catalog bundled with this release: ${catalog.model_count} models from ${catalog.vendor_count} vendors, published ${formatDate(catalog.published_at)}. A fetched catalog must carry a valid signature at its URL plus .sig.`;
}
