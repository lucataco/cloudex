// Where a sign-in may send a browser next: one of the dashboard's own pages
// (apps/dashboard/src/main.tsx), never an API route. A same-origin path to
// /v1/sandboxes/x/files/content would turn a crafted link into a download
// from a sandbox. The API checks the links it makes with the same rule
// (CreateLoginLink), and the login page checks next before going there. No
// imports, so that the dashboard can use it without bundling zod.

// The pages: /, /saves, /nodes, /access and /s/<sandbox>[/<tab>...]. The
// query is the page's own, such as a files page's ?path=.
const PAGE = /^\/(saves|nodes|access|s\/[A-Za-z0-9._~%-]+(\/[A-Za-z0-9._~%-]+)*\/?)?$/;
const QUERY = /^[A-Za-z0-9._~\/?=&%-]*$/;
// . and .. segments, %2e spelled or not, which a browser resolves away.
const DOTS = /^(\.|%2e)+$/i;

export function isDashboardPath(p: string): boolean {
  if (p.length > 2000) return false;
  const q = p.indexOf("?");
  const path = q < 0 ? p : p.slice(0, q);
  if (!PAGE.test(path) || (q >= 0 && !QUERY.test(p.slice(q + 1)))) return false;
  return !path.split("/").some((s) => DOTS.test(s));
}

// next as given when it's a dashboard page, or else the list of sandboxes.
export const safeNext = (n: string | null | undefined) => (n && isDashboardPath(n) ? n : "/");
