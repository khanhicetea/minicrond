export const basePath = new URL(document.querySelector('base')?.getAttribute('href') ?? '/', window.location.origin).pathname.replace(/\/$/, '');

export const publicPath = (path: string) => basePath + path;
export const publicURL = (path: string) => window.location.origin + publicPath(path);

/** UI traffic is confined to the gateway's API route, including downloads. */
export function apiPath(path: string): string {
  const resolved = new URL(publicPath(path), window.location.origin);
  if (!path.startsWith('/api/v1/') || resolved.origin !== window.location.origin ||
      !resolved.pathname.startsWith(publicPath('/api/v1/'))) {
    throw new TypeError('UI requests must stay under /api/v1/');
  }
  return resolved.pathname + resolved.search;
}
