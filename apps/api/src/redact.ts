// Keeping a provider key out of what a sandbox sees (egress.ts). Some
// providers quote a key they refuse in their error, in full or in part
// (OpenAI shows its first and last characters). An error answer to a request
// that carried a key Cloudex added has every copy of that key, and of its
// distinctive middle, taken out of its headers and its body before the
// sandbox gets it. A body too big to read whole is replaced. Successful
// answers aren't touched, so streams pass as they come.
export const ERROR_BODY_MAX = 256 * 1024;

async function readCapped(body: ReadableStream<Uint8Array>, max: number): Promise<string | null> {
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > max) {
      await reader.cancel().catch(() => {});
      return null;
    }
    chunks.push(value);
  }
  return new TextDecoder().decode(await new Blob(chunks).arrayBuffer());
}

export async function withoutKey(res: Response, key: string): Promise<Response> {
  if (key.length < 8) return res;
  const parts = [key, key.length > 24 ? key.slice(8, -4) : null].filter((p): p is string => Boolean(p));
  const scrub = (s: string) => parts.reduce((t, p) => t.split(p).join("[redacted]"), s);
  const headers = new Headers();
  for (const [k, v] of res.headers) headers.append(k, scrub(v));
  headers.delete("content-length");
  headers.delete("content-encoding");
  const text = res.body ? await readCapped(res.body, ERROR_BODY_MAX).catch(() => null) : "";
  if (text === null) {
    headers.set("content-type", "application/json");
    return new Response(JSON.stringify({ type: "error", error: { type: "api_error", message: `the provider answered ${res.status}` } }), {
      status: res.status,
      headers,
    });
  }
  return new Response(scrub(text), { status: res.status, statusText: res.statusText, headers });
}
