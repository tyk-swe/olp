import { describe, expect, it, vi } from 'vitest';
import {
  bookmarkOrigin,
  installationOrigin,
  readInstallations,
  saveInstallations
} from './installations';

describe('installation bookmarks', () => {
  it.each([
    ['https://Gateway.Example.test:443/', 'https://gateway.example.test'],
    ['https://gateway.example.test:8443', 'https://gateway.example.test:8443'],
    ['http://localhost:8080', 'http://localhost:8080'],
    ['http://127.0.0.1:8080', 'http://127.0.0.1:8080'],
    ['http://[::1]:8080', 'http://[::1]:8080']
  ])('accepts and normalizes the installation origin %s', (value, expected) => {
    expect(installationOrigin(value)).toBe(expected);
  });

  it.each([
    'http://gateway.example.test',
    'https://user:password@gateway.example.test',
    'https://gateway.example.test/path',
    'https://gateway.example.test/?query=value',
    'https://gateway.example.test/#fragment',
    'file:///tmp/installation',
    'not a URL'
  ])('rejects an unsafe installation origin %s', (value) => {
    expect(() => installationOrigin(value)).toThrow();
  });

  it('rejects another installation on the same hostname because cookies are shared across ports', () => {
    const current = 'https://current.example.test';
    const bookmarks = [
      { name: 'Other', origin: 'https://other.example.test:8443' }
    ];

    expect(bookmarkOrigin(current, current, bookmarks)).toBe(current);
    expect(bookmarkOrigin('https://new.example.test', current, bookmarks)).toBe(
      'https://new.example.test'
    );
    expect(() =>
      bookmarkOrigin('https://current.example.test:8443', current, bookmarks)
    ).toThrow(/distinct hostname/i);
    expect(() =>
      bookmarkOrigin('https://other.example.test:9443', current, bookmarks)
    ).toThrow(/distinct hostname/i);
  });

  it('restores valid bookmarks with normalized names, origins and duplicate removal', () => {
    const storage = {
      getItem: vi
        .fn()
        .mockReturnValue(
          JSON.stringify([
            { name: ' Gateway ', origin: 'https://GATEWAY.example.test:443/' },
            { name: 'Duplicate', origin: 'https://gateway.example.test' },
            { name: ' ', origin: 'https://blank.example.test' },
            { name: 'Other', origin: 'https://other.example.test' },
            { name: 2, origin: 'https://invalid.example.test' },
            null
          ])
        )
    };

    expect(readInstallations(storage)).toEqual([
      { name: 'Gateway', origin: 'https://gateway.example.test' },
      { name: 'Other', origin: 'https://other.example.test' }
    ]);
  });

  it('ignores unreadable or malformed saved bookmarks', () => {
    expect(readInstallations({ getItem: () => '{broken' })).toEqual([]);
    expect(
      readInstallations({ getItem: () => '{"name":"not a list"}' })
    ).toEqual([]);
    expect(
      readInstallations({
        getItem: () => {
          throw new Error('storage unavailable');
        }
      })
    ).toEqual([]);
  });

  it('bounds the number of saved bookmarks it reads', () => {
    const saved = Array.from({ length: 25 }, (_, index) => ({
      name: `Gateway ${index}`,
      origin: `https://gateway-${index}.example.test`
    }));
    const restored = readInstallations({
      getItem: () => JSON.stringify(saved)
    });

    expect(restored.length).toBeLessThan(saved.length);
    expect(restored).toEqual(saved.slice(0, restored.length));
  });

  it('saves bookmarks so the same values can be restored', () => {
    const bookmarks = [
      { name: 'Gateway', origin: 'https://gateway.example.test' }
    ];
    const setItem = vi.fn<(key: string, value: string) => void>();
    saveInstallations({ setItem }, bookmarks);
    const [key, value] = setItem.mock.lastCall ?? ['', ''];

    expect(setItem).toHaveBeenCalledOnce();
    expect(
      readInstallations({
        getItem: (requested) => (requested === key ? value : null)
      })
    ).toEqual(bookmarks);
  });
});
