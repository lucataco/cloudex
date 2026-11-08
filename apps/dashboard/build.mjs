// Builds the dashboard into dist/assets.js, one module the API Worker imports
// (apps/api/src/dashboard.ts) so that its files end up inside worker.js: the
// app's script and stylesheet (Preact, xterm.js and the pages), noVNC's
// script (loaded only by the desktop page), and the two fonts, in base64.
//   node build.mjs [--dev]   --dev: not minified
import { build } from "esbuild";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const out = path.join(here, "dist");
const dev = process.argv.includes("--dev");
fs.rmSync(out, { recursive: true, force: true });
fs.mkdirSync(out, { recursive: true });

const common = {
  bundle: true,
  format: "esm",
  target: ["chrome111", "firefox115", "safari16.4"],
  minify: !dev,
  legalComments: "eof",
  write: false,
  logLevel: "warning",
};

const app = await build({
  ...common,
  entryPoints: { app: path.join(here, "src/main.tsx") },
  outdir: out,
  jsx: "automatic",
  jsxImportSource: "preact",
  // The fonts are served beside the stylesheet, under these names.
  external: ["/assets/*"],
  define: { "process.env.NODE_ENV": dev ? '"development"' : '"production"' },
});
const vnc = await build({ ...common, entryPoints: { vnc: path.join(here, "src/vnc.ts") }, outdir: out });

const text = (name) => [...app.outputFiles, ...vnc.outputFiles].find((f) => path.basename(f.path) === name).text;
const font = (from) => fs.readFileSync(path.join(here, "node_modules", from)).toString("base64");
const files = {
  appJs: text("app.js"),
  appCss: text("app.css"),
  vncJs: text("vnc.js"),
  archivo: font("@fontsource-variable/archivo/files/archivo-latin-wght-normal.woff2"),
  martianMono: font("@fontsource-variable/martian-mono/files/martian-mono-latin-wght-normal.woff2"),
};
let js = "// Built by apps/dashboard/build.mjs. Fonts are base64.\n";
for (const [name, body] of Object.entries(files)) {
  js += `export const ${name} = ${JSON.stringify(body)};\n`;
  console.log(`${name.padEnd(12)} ${(body.length / 1024).toFixed(0).padStart(5)} KB`);
}
fs.writeFileSync(path.join(out, "assets.js"), js);
