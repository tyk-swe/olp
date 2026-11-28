export class LatestRun<T> {
  private controller: AbortController | null = null;
  private result: Promise<T> | null = null;

  pending(): Promise<T> | null {
    return this.result;
  }

  start(run: (signal: AbortSignal) => Promise<T>): Promise<T> {
    this.abort();
    const controller = new AbortController();
    this.controller = controller;
    const result = run(controller.signal).finally(() => {
      if (this.controller !== controller) return;
      this.controller = null;
      this.result = null;
    });
    this.result = result;
    return result;
  }

  interrupt(): void {
    this.controller?.abort();
  }

  abort(): void {
    this.interrupt();
    this.controller = null;
    this.result = null;
  }
}
