/** Trigger a browser download for an authenticated URL. */
export async function downloadFile(url: string, filename: string, fetchBlob: (url: string) => Promise<Blob>) {
  const blob = await fetchBlob(url);
  const link = document.createElement('a');
  link.href = URL.createObjectURL(blob);
  link.download = filename;
  link.click();
  URL.revokeObjectURL(link.href);
}
