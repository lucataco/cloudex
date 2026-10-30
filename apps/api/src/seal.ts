// AES-GCM under a key derived (HKDF) from the admin key, so a copy of the D1
// database or of a Durable Object's storage alone doesn't reveal what's sealed.
// Each use has its own label. Replacing the admin key (cloudex deploy --yes)
// makes everything sealed before unreadable.
async function cipherKey(adminKey: string, label: string) {
  const base = await crypto.subtle.importKey("raw", new TextEncoder().encode(adminKey), "HKDF", false, ["deriveKey"]);
  return crypto.subtle.deriveKey(
    { name: "HKDF", hash: "SHA-256", salt: new TextEncoder().encode("cloudex"), info: new TextEncoder().encode(label) },
    base,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"],
  );
}

// In chunks: spreading a large array into fromCharCode overflows the stack.
function b64(bytes: Uint8Array) {
  let s = "";
  for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(s);
}
const unb64 = (text: string) => Uint8Array.from(atob(text), (c) => c.charCodeAt(0));

export async function seal(adminKey: string, label: string, plain: string) {
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const key = await cipherKey(adminKey, label);
  const sealed = await crypto.subtle.encrypt({ name: "AES-GCM", iv }, key, new TextEncoder().encode(plain));
  return `v1:${b64(iv)}:${b64(new Uint8Array(sealed))}`;
}

// Throws when the value was sealed under another admin key.
export async function open(adminKey: string, label: string, value: string) {
  const [version, iv, data] = value.split(":");
  if (version !== "v1" || !iv || !data) throw new Error("unknown sealed format");
  const key = await cipherKey(adminKey, label);
  const plain = await crypto.subtle.decrypt({ name: "AES-GCM", iv: unb64(iv) }, key, unb64(data));
  return new TextDecoder().decode(plain);
}
