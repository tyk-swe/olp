import { describe, expect, it, vi } from 'vitest';
import {
  addBookmark,
  bookmarkOrigin,
  installationOrigin,
  readInstallations,
  removeBookmark,
  saveInstallations,
  storedBookmarks
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

describe('installation bookmark helpers', () => {
  it('reads bookmarks through the accessor only inside the result capture', () => {
    const getItem = vi
      .fn()
      .mockReturnValue(
        JSON.stringify([
          { name: 'Other', origin: 'https://other.example.test' }
        ])
      );
    const result = storedBookmarks(
      () => ({ getItem }),
      'https://current.example.test'
    );

    expect(result).toEqual({
      ok: true,
      value: [{ name: 'Other', origin: 'https://other.example.test' }]
    });
    expect(getItem).toHaveBeenCalledOnce();
  });

  it('reports a storage failure as a result instead of throwing', () => {
    const result = storedBookmarks(() => {
      throw new Error('storage blocked');
    }, 'https://current.example.test');

    expect(result.ok).toBe(false);
  });

  it('drops stored bookmarks that would share a hostname with the current installation', () => {
    const result = storedBookmarks(
      () => ({
        getItem: () =>
          JSON.stringify([
            { name: 'Same host', origin: 'https://current.example.test:8443' },
            { name: 'Other', origin: 'https://other.example.test' }
          ])
      }),
      'https://current.example.test'
    );

    expect(result).toEqual({
      ok: true,
      value: [{ name: 'Other', origin: 'https://other.example.test' }]
    });
  });

  it('adds and persists a validated bookmark, returning the saved list', () => {
    const setItem = vi.fn();
    const result = addBookmark(
      () => ({ setItem }),
      'https://current.example.test',
      [{ name: 'Old', origin: 'https://old.example.test' }],
      ' New ',
      'https://NEW.example.test:443/'
    );

    expect(result).toEqual({
      ok: true,
      value: [
        { name: 'Old', origin: 'https://old.example.test' },
        { name: 'New', origin: 'https://new.example.test' }
      ]
    });
    expect(setItem).toHaveBeenCalledOnce();
  });

  it('rejects an invalid bookmark without touching storage', () => {
    const setItem = vi.fn();
    const result = addBookmark(
      () => ({ setItem }),
      'https://current.example.test',
      [],
      '   ',
      'https://new.example.test'
    );

    expect(result.ok).toBe(false);
    expect(setItem).not.toHaveBeenCalled();
  });

  it('removes and persists a bookmark', () => {
    const setItem = vi.fn();
    const bookmarks = [
      { name: 'Old', origin: 'https://old.example.test' },
      { name: 'Other', origin: 'https://other.example.test' }
    ];
    const result = removeBookmark(
      () => ({ setItem }),
      bookmarks,
      'https://old.example.test'
    );

    expect(result).toEqual({
      ok: true,
      value: [{ name: 'Other', origin: 'https://other.example.test' }]
    });
    expect(setItem).toHaveBeenCalledOnce();
  });
});
