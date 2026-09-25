export const basePath = new URL(document.querySelector('base')?.getAttribute('href') ?? '/', window.location.origin).pathname.replace(/\/$/, '');

export const publicPath = (path: string) => basePath + path;
export const publicURL = (path: string) => window.location.origin + publicPath(path);
