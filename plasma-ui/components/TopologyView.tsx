"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Background,
  Controls,
  MiniMap,
  ReactFlow,
  type Edge,
  type Node,
} from "@xyflow/react";
import type { Topology } from "@/lib/plasma";
import { nodeTypes } from "./nodes";

const POLL_MS = 2000;

// Laying VMs out in rows under the bridge keeps a 50-VM fleet readable, which a
// circular arrangement stops doing well past a dozen nodes.
const PER_ROW = 4;
const COL_W = 250;
const ROW_H = 190;

export default function TopologyView() {
  const [topo, setTopo] = useState<Topology | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [toast, setToast] = useState<{ msg: string; err?: boolean } | null>(null);
  const [filter, setFilter] = useState("");
  const [busy, setBusy] = useState(false);

  const notify = useCallback((msg: string, err = false) => {
    setToast({ msg, err });
    setTimeout(() => setToast(null), err ? 8000 : 4000);
  }, []);

  // Poll so status, uptime and newly launched VMs stay current.
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      try {
        const res = await fetch("/api/topology", { cache: "no-store" });
        const body = await res.json();
        if (!alive) return;
        if (!res.ok) {
          setError(body.error ?? "failed to reach plasmad");
        } else {
          setTopo(body as Topology);
          setError(null);
        }
      } catch (e) {
        if (alive) setError((e as Error).message);
      }
    };
    tick();
    const t = setInterval(tick, POLL_MS);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []);

  const captureQuery = useMemo(() => {
    const p = new URLSearchParams();
    if (filter.trim()) p.set("filter", filter.trim());
    const qs = p.toString();
    return qs ? `?${qs}` : "";
  }, [filter]);

  const onWireshark = useCallback(
    async (id: string) => {
      setBusy(true);
      try {
        const res = await fetch(`/api/capture/${encodeURIComponent(id)}/wireshark`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ filter: filter.trim() }),
        });
        const body = await res.json();
        if (!res.ok) notify(body.error ?? "failed to start Wireshark", true);
        else notify(`Wireshark opened on ${id}`);
      } catch (e) {
        notify((e as Error).message, true);
      } finally {
        setBusy(false);
      }
    },
    [filter, notify],
  );

  const onDownload = useCallback(
    (id: string) => {
      // The stream only ends when the download is cancelled, which is what
      // stops the capture — so say so rather than let it look stuck.
      window.location.href = `/api/capture/${encodeURIComponent(id)}${captureQuery}`;
      notify(`Capturing ${id} to a .pcap — cancel the download to stop it`);
    },
    [captureQuery, notify],
  );

  const onCopy = useCallback(
    async (id: string) => {
      const f = filter.trim() ? ` -f '${filter.trim()}'` : "";
      const cmd = `plasma vm capture ${id}${f} --wireshark`;
      try {
        await navigator.clipboard.writeText(cmd);
        notify(`Copied: ${cmd}`);
      } catch {
        notify(cmd);
      }
    },
    [filter, notify],
  );

  const { nodes, edges } = useMemo<{ nodes: Node[]; edges: Edge[] }>(() => {
    if (!topo) return { nodes: [], edges: [] };

    const plane = topo.planes[0];
    const vms = topo.nodes;
    const rowCount = Math.ceil(vms.length / PER_ROW) || 1;
    const widest = Math.min(vms.length, PER_ROW) || 1;
    const centerX = ((widest - 1) * COL_W) / 2;

    const nodes: Node[] = [];

    if (plane) {
      nodes.push({
        id: plane.bridge,
        type: "bridge",
        position: { x: centerX - 20, y: 0 },
        data: { plane, vmCount: vms.length, onWireshark, onDownload, onCopy, busy },
        draggable: true,
      });
    }

    vms.forEach((vm, i) => {
      const row = Math.floor(i / PER_ROW);
      const col = i % PER_ROW;
      const inRow = Math.min(vms.length - row * PER_ROW, PER_ROW);
      const rowOffset = ((widest - inRow) * COL_W) / 2;
      nodes.push({
        id: vm.id,
        type: "vm",
        position: { x: col * COL_W + rowOffset, y: 220 + row * ROW_H },
        data: { vm, onWireshark, onDownload, onCopy, busy },
        draggable: true,
      });
    });

    // Declared links come from a topology file and have no L2 segment behind
    // them yet, so they must not look like the real tap-to-bridge links.
    const edges: Edge[] = topo.links.map((l, i) => ({
      id: `${l.from}-${l.to}-${i}`,
      source: l.from,
      target: l.to,
      label: l.wired ? l.via : l.label,
      animated: l.wired && vms.find((v) => v.id === l.from)?.status === "running",
      style: l.wired
        ? { stroke: "#30363d" }
        : { stroke: "#d29922", strokeDasharray: "6 4", strokeWidth: 1.5 },
      labelStyle: {
        fill: l.wired ? "#8b949e" : "#d29922",
        fontSize: 10,
        fontFamily: "monospace",
      },
      labelBgStyle: { fill: "#0d1117" },
    }));

    void rowCount;
    return { nodes, edges };
  }, [topo, onWireshark, onDownload, onCopy, busy]);

  const running = topo?.nodes.filter((n) => n.status === "running").length ?? 0;

  return (
    <div className="app">
      <header className="header">
        <div className="brand">
          Plasma <span>topology &amp; capture</span>
        </div>

        <div className="filter-box">
          <label htmlFor="bpf">filter</label>
          <input
            id="bpf"
            value={filter}
            placeholder="BPF, e.g. icmp"
            onChange={(e) => setFilter(e.target.value)}
            spellCheck={false}
          />
        </div>

        <div className="spacer" />

        <span className={`pill ${error ? "down" : "live"}`}>
          {error ? "plasmad unreachable" : `${running} running`}
        </span>
      </header>

      {error && (
        <p className="banner">
          {error}
          {error.includes("not running") && (
            <>
              {" "}
              Start it with <code>sudo plasmad &amp;</code>.
            </>
          )}
        </p>
      )}

      <div className="canvas">
        <ReactFlow
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          fitView
          minZoom={0.2}
          proOptions={{ hideAttribution: true }}
        >
          <Background color="#21262d" gap={20} />
          <Controls />
          <MiniMap
            pannable
            zoomable
            maskColor="rgba(13,17,23,0.75)"
            // Nodes need to out-contrast the dark canvas or the minimap reads
            // as an empty box.
            nodeColor={(n) =>
              n.type === "bridge" ? "#58a6ff" : "#3fb950"
            }
            style={{ background: "#161b22", border: "1px solid #30363d" }}
          />
        </ReactFlow>

        {topo && topo.nodes.length === 0 && !error && (
          <div className="empty">
            <div>No VMs in the fleet.</div>
            <div>
              Launch one with <code>plasma vm launch host</code>.
            </div>
          </div>
        )}

        {toast && (
          <div className={`toast ${toast.err ? "err" : ""}`}>{toast.msg}</div>
        )}
      </div>
    </div>
  );
}
