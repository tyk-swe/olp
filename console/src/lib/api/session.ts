let csrfToken: string | null = null;
let csrfTokenVersion = 0;

export function setCsrfToken(token: string) {
  csrfToken = token;
  csrfTokenVersion += 1;
}

export function getCsrfToken() {
  return csrfToken;
}

export function getCsrfTokenVersion() {
  return csrfTokenVersion;
}

export function clearCsrfToken() {
  csrfToken = null;
  csrfTokenVersion += 1;
}
