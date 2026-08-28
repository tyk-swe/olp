import { hashKey, type QueryClient } from '@tanstack/svelte-query';

/**
 * Keeps one principal's cached queries from ever being served to another.
 * Every query key is hashed under the current partition name, so rotating
 * the name on sign-in, sign-out, or a principal change orphans the previous
 * principal's entries even before the cache is cleared.
 */
export class QueryPartition {
  private client: QueryClient | null = null;
  private generation = 0;
  private name = 'anonymous:0';

  attach(client: QueryClient): () => void {
    this.client = client;
    return () => {
      if (this.client === client) this.client = null;
    };
  }

  current(): string {
    return this.name;
  }

  keyHash(key: readonly unknown[]): string {
    return `${this.name}|${hashKey(key)}`;
  }

  use(name: string): void {
    this.name = name;
  }

  rotateAnonymous(): void {
    this.name = `anonymous:${++this.generation}`;
  }

  async cancelAndClear(): Promise<void> {
    const client = this.client;
    if (!client) return;
    try {
      await client.cancelQueries();
    } finally {
      client.clear();
    }
  }
}
