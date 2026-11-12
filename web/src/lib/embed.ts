/**
 * Embedded mode: the UI runs inside another site's iframe (around 1000px wide).
 * Detected from `?embed=1` (remembered for the tab) or from being framed.
 * Layout tightens the header and panes via `html[data-embed="true"]`.
 */
function detect(): boolean {
  try {
    if (new URLSearchParams(window.location.search).get('embed') === '1') {
      sessionStorage.setItem('minicron_embed', '1');
      return true;
    }
    if (sessionStorage.getItem('minicron_embed') === '1') return true;
    return window.self !== window.top;
  } catch {
    // Sandboxed frames can throw on storage access; treat them as embedded.
    return true;
  }
}

export const isEmbedded = typeof window !== 'undefined' && detect();
