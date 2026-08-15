import http from "node:http";
import type { Readable } from "node:stream";

// All plasmad access goes through its unix socket. That is deliberate: the
// socket is root:plasma 0660, so the OS group check is what authorises this UI.
// Giving plasmad a TCP listener instead would expose a root daemon's API to
// every local user, so we never do that — this server just has to run as
// someone in the "plasma" group, exactly like the CLI.
export const SOCKET_PATH = process.env.PLASMA_SOCKET ?? "/var/run/plasma.sock";

export type VmStatus = "running" | "exited";

export interface Plane {
  name: string;
  bridge: string;
  subnet: string;
  gateway: string;
  capture_target: string;
  capture_note: string;
}

export interface TopologyNode {
  id: string;
  group: string;
  role: string;
  status: VmStatus;
  ip: string;
  mac: string;
  tap: string;
  pid: number;
  capturable: boolean;
  uptime_secs: number;
  plane: string;
}

export interface TopologyLink {
  from: string;
  to: string;
  via: string;
  plane: string;
}

export interface Topology {
  planes: Plane[];
  nodes: TopologyNode[];
  links: TopologyLink[];
}

export class PlasmaError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

/** Turn transport-level failures into something actionable in the UI. */
function describeDialError(err: NodeJS.ErrnoException): PlasmaError {
  if (err.code === "ENOENT" || err.code === "ECONNREFUSED") {
    return new PlasmaError(
      `plasmad is not running (no listener on ${SOCKET_PATH})`,
      503,
    );
  }
  if (err.code === "EACCES") {
    return new PlasmaError(
      `no access to ${SOCKET_PATH} — add yourself to the "plasma" group ` +
        `(sudo usermod -aG plasma $USER, then re-login)`,
      403,
    );
  }
  return new PlasmaError(err.message, 502);
}

/** Issue a request to plasmad and hand back the raw response stream. */
function request(
  path: string,
  signal?: AbortSignal,
): Promise<http.IncomingMessage> {
  return new Promise((resolve, reject) => {
    const req = http.request(
      { socketPath: SOCKET_PATH, path, method: "GET" },
      resolve,
    );
    req.on("error", (err) => reject(describeDialError(err)));
    if (signal) {
      // Abort tears down the socket, which is what stops a running capture on
      // the daemon side.
      signal.addEventListener("abort", () => req.destroy(), { once: true });
    }
    req.end();
  });
}

async function readAll(res: http.IncomingMessage): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of res) chunks.push(chunk as Buffer);
  return Buffer.concat(chunks).toString("utf8");
}

export async function getJSON<T>(path: string): Promise<T> {
  const res = await request(path);
  const body = await readAll(res);
  if (res.statusCode !== 200) {
    throw new PlasmaError(body.trim() || res.statusMessage || "request failed", res.statusCode ?? 502);
  }
  return JSON.parse(body) as T;
}

export function getTopology(): Promise<Topology> {
  return getJSON<Topology>("/v1/topology");
}

export interface CaptureQuery {
  filter?: string;
  snaplen?: number;
}

function capturePath(id: string, q: CaptureQuery): string {
  const params = new URLSearchParams();
  if (q.filter) params.set("filter", q.filter);
  if (q.snaplen && q.snaplen > 0) params.set("snaplen", String(q.snaplen));
  const qs = params.toString();
  return `/v1/vms/${encodeURIComponent(id)}/capture${qs ? `?${qs}` : ""}`;
}

/**
 * Open a live pcap stream. Errors surface before any packets flow because
 * plasmad only returns 200 once tcpdump is actually capturing.
 */
export async function openCapture(
  id: string,
  q: CaptureQuery = {},
  signal?: AbortSignal,
): Promise<Readable> {
  const res = await request(capturePath(id, q), signal);
  if (res.statusCode !== 200) {
    const body = await readAll(res);
    throw new PlasmaError(body.trim() || "capture failed", res.statusCode ?? 502);
  }
  return res;
}
