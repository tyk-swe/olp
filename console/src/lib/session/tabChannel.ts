export type TabMessage = 'changed' | 'rotated';

export class TabChannel {
  private channel: BroadcastChannel | null = null;

  connect(received: Readonly<Record<TabMessage, () => void>>): () => void {
    if (typeof BroadcastChannel === 'undefined') return () => {};
    const channel = new BroadcastChannel('olp-authentication');
    this.channel = channel;
    channel.onmessage = ({ data }: MessageEvent<unknown>) => {
      if (data === 'changed' || data === 'rotated') received[data]();
    };
    return () => {
      channel.close();
      this.channel = null;
    };
  }

  announce(message: TabMessage): void {
    this.channel?.postMessage(message);
  }

  close(): void {
    this.channel?.close();
    this.channel = null;
  }
}
