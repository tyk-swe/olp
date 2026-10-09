import { expect, it } from 'vitest';
import {
  installationOrigin,
  bookmarkOrigin,
  readInstallations,
  saveInstallations
} from './installations';

it('keeps bookmarks secret free and requires an installation origin', () => {
  for (const origin of [
    'https://user:password@example.com',
    'https://example.com/path',
    'https://example.com?token=secret',
    'http://example.com',
    'javascript:alert(1)'
  ])
    expect(() => installationOrigin(origin)).toThrow();
  expect(installationOrigin('https://example.com/')).toBe(
    'https://example.com'
  );
  expect(installationOrigin('http://localhost:3000')).toBe(
    'http://localhost:3000'
  );
  const storage = {
    value: '',
    setItem(_key: string, value: string) {
      this.value = value;
    },
    getItem() {
      return this.value;
    }
  };
  saveInstallations(storage, [
    { name: 'Second installation', origin: 'https://second.example.com' }
  ]);
  expect(readInstallations(storage)).toEqual([
    { name: 'Second installation', origin: 'https://second.example.com' }
  ]);
});

it('requires distinct hostnames so sessions cannot collide across ports', () => {
  expect(() =>
    bookmarkOrigin(
      'https://first.example.com:8443',
      'https://first.example.com',
      []
    )
  ).toThrow('cookies are shared across ports');
  expect(
    bookmarkOrigin(
      'https://second.example.com',
      'https://first.example.com',
      []
    )
  ).toBe('https://second.example.com');
  expect(() =>
    bookmarkOrigin(
      'https://second.example.com:8443',
      'https://first.example.com',
      [{ name: 'Second', origin: 'https://second.example.com' }]
    )
  ).toThrow();
});
