// A TCP proxy that counts bytes in each direction, for measuring what one
// browser tab really costs in egress (HTTP polls and WebSocket frames as
// they go over the wire, after compression).
//
// Usage: node scripts/count-proxy.mjs <listen-port> <target-host:port> <seconds>
// Point the frontend at the listen port (TAPE_DEV_API for `npm run dev`).

import net from "node:net";

const [listen, target, seconds] = [Number(process.argv[2]), process.argv[3], Number(process.argv[4])];
const [host, port] = target.split(":");
let down = 0;
let up = 0;

const server = net.createServer((client) => {
  const upstream = net.connect(Number(port), host);
  client.on("data", (d) => {
    up += d.length;
    upstream.write(d);
  });
  upstream.on("data", (d) => {
    down += d.length;
    client.write(d);
  });
  const end = () => {
    client.destroy();
    upstream.destroy();
  };
  client.on("close", end).on("error", end);
  upstream.on("close", end).on("error", end);
});

server.listen(listen, () => {
  const started = Date.now();
  setTimeout(() => {
    const s = (Date.now() - started) / 1000;
    console.log(`seconds=${s.toFixed(1)} server_to_client_bytes=${down} client_to_server_bytes=${up}`);
    console.log(`server_to_client=${(down / s / 1024).toFixed(2)} KB/s (${((down / s) * 3600 / 1024 / 1024).toFixed(1)} MB/h)`);
    process.exit(0);
  }, seconds * 1000);
});
