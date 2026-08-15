"use client";

import { Handle, Position, type NodeProps } from "@xyflow/react";
import type { Plane, TopologyNode } from "@/lib/plasma";

export interface VmNodeData extends Record<string, unknown> {
  vm: TopologyNode;
  onWireshark: (id: string) => void;
  onDownload: (id: string) => void;
  onCopy: (id: string) => void;
  busy: boolean;
}

export interface BridgeNodeData extends Record<string, unknown> {
  plane: Plane;
  vmCount: number;
  onWireshark: (id: string) => void;
  onDownload: (id: string) => void;
  onCopy: (id: string) => void;
  busy: boolean;
}

function uptime(secs: number): string {
  if (secs <= 0) return "—";
  if (secs < 60) return `${secs}s`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m ${secs % 60}s`;
  return `${Math.floor(secs / 3600)}h ${Math.floor((secs % 3600) / 60)}m`;
}

export function VmNode({ data }: NodeProps) {
  const { vm, onWireshark, onDownload, onCopy, busy } = data as VmNodeData;
  const running = vm.status === "running";

  return (
    <div className={`node ${vm.status}`}>
      <Handle type="source" position={Position.Top} />
      <div className="node-title">
        <span className={`dot ${vm.status}`} />
        {vm.id}
        <span className="badge">{vm.group}</span>
      </div>

      <div className="node-meta">
        <span>ip</span>
        <b>{vm.ip || "—"}</b>
        <span>tap</span>
        <b>{vm.tap}</b>
        <span>uptime</span>
        <b>{running ? uptime(vm.uptime_secs) : vm.status}</b>
      </div>

      <div className="node-actions">
        <button
          className="act primary"
          disabled={!vm.capturable || busy}
          onClick={() => onWireshark(vm.id)}
          title={
            vm.capturable
              ? "Open a live capture in Wireshark"
              : "VM is not running"
          }
        >
          Wireshark
        </button>
        <button
          className="act"
          disabled={!vm.capturable}
          onClick={() => onDownload(vm.id)}
          title="Stream this capture to a .pcap file"
        >
          .pcap
        </button>
        <button className="act" onClick={() => onCopy(vm.id)} title="Copy the CLI command">
          Copy
        </button>
      </div>
    </div>
  );
}

export function BridgeNode({ data }: NodeProps) {
  const { plane, vmCount, onWireshark, onDownload, onCopy, busy } =
    data as BridgeNodeData;

  return (
    <div className="node bridge">
      <div className="node-title">
        <span className="dot running" />
        {plane.bridge}
        <span className="badge">{plane.name} plane</span>
      </div>

      <div className="node-meta">
        <span>subnet</span>
        <b>{plane.subnet}</b>
        <span>gateway</span>
        <b>{plane.gateway}</b>
        <span>vms</span>
        <b>{vmCount}</b>
      </div>

      <div className="node-actions">
        <button
          className="act"
          disabled={busy}
          onClick={() => onWireshark(plane.capture_target)}
        >
          Wireshark
        </button>
        <button className="act" onClick={() => onDownload(plane.capture_target)}>
          .pcap
        </button>
        <button className="act" onClick={() => onCopy(plane.capture_target)}>
          Copy
        </button>
      </div>

      {/* Stated plainly so nobody reads a bridge capture as a fleet-wide tap. */}
      <p className="node-note">{plane.capture_note}</p>

      <Handle type="target" position={Position.Bottom} />
    </div>
  );
}

export const nodeTypes = { vm: VmNode, bridge: BridgeNode };
