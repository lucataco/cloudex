export const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

// Container calls can hang (start() does while a new image propagates), so every
// one of them gets a deadline.
export function withTimeout<T>(p: Promise<T>, ms: number, what: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const deadline = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} timed out after ${Math.round(ms / 1000)} s`)), ms);
  });
  return Promise.race([p, deadline]).finally(() => timer && clearTimeout(timer));
}

const ALPHABET = "abcdefghijklmnopqrstuvwxyz0123456789";

function randomString(n: number) {
  const bytes = crypto.getRandomValues(new Uint8Array(n));
  return [...bytes].map((b) => ALPHABET[b % ALPHABET.length]).join("");
}

export const newSandboxId = () => `cx-${randomString(6)}`;

export const isSandboxId = (s: string) => /^cx-[a-z0-9]{6}$/.test(s);

// Errors that cross the Durable Object RPC boundary keep only their message, so
// the HTTP status rides along as a "[409] ..." prefix.
export function fail(status: number, message: string): never {
  throw new Error(`[${status}] ${message}`);
}

export function errorStatus(e: unknown): { status: number; message: string } {
  const message = e instanceof Error ? e.message : String(e);
  const m = /^\[(\d{3})\] (.*)$/s.exec(message);
  return m ? { status: Number(m[1]), message: m[2]! } : { status: 500, message };
}

// Unguessable, stable token for a preview link: HMAC of sandbox and port.
export async function previewToken(secret: string, id: string, port: number) {
  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const sig = new Uint8Array(await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(`preview:${id}:${port}`)));
  return [...sig.slice(0, 10)].map((b) => ALPHABET[b % ALPHABET.length]).join("");
}

// Compares digests so the check takes the same time whatever the input.
export async function sameSecret(given: string, expected: string): Promise<boolean> {
  const enc = new TextEncoder();
  const a = await crypto.subtle.digest("SHA-256", enc.encode(given));
  const b = await crypto.subtle.digest("SHA-256", enc.encode(expected));
  return crypto.subtle.timingSafeEqual(a, b);
}
