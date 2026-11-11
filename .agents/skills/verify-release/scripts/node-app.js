// An app for the node area's preview checks (verify.sh N10), run in the machine
// as node node-app.js PORT: GET answers its path, POST answers the body's
// length and MD5, and /ws is a WebSocket that echoes short text messages.
const http = require("http");
const crypto = require("crypto");
const s = http.createServer((req, res) => {
  let n = 0;
  const h = crypto.createHash("md5");
  req.on("data", (d) => {
    n += d.length;
    h.update(d);
  });
  req.on("end", () => res.end(req.method === "POST" ? `posted:${n}:${h.digest("hex")}` : `hello-preview ${req.url}`));
});
s.on("upgrade", (req, sock) => {
  const accept = crypto.createHash("sha1").update(req.headers["sec-websocket-key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest("base64");
  sock.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
  sock.on("data", (b) => {
    const op = b[0] & 15;
    const len = b[1] & 127;
    const mask = b.subarray(2, 6);
    const p = Buffer.from(b.subarray(6, 6 + len));
    for (let i = 0; i < p.length; i++) p[i] ^= mask[i % 4];
    if (op === 8) return sock.end();
    const out = Buffer.from(`echo:${p.toString()}`);
    sock.write(Buffer.concat([Buffer.from([0x81, out.length]), out]));
  });
});
s.listen(Number(process.argv[2] || 3000), "0.0.0.0");
