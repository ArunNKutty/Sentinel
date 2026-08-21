import { spawn } from "node:child_process";
import { NextRequest, NextResponse } from "next/server";
import { openCapture, PlasmaError } from "@/lib/plasma";

export const dynamic = "force-dynamic";

/**
 * Launch Wireshark on the operator's desktop, fed by a live capture.
 *
 * This works because the UI server runs as the logged-in user rather than as
 * root — it inherits their DISPLAY and can open a GUI. A browser cannot spawn
 * a local program, and plasmad (root, headless) has no desktop session, so
 * this route is the only place the handoff can happen.
 */
export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ id: string }> },
) {
  const { id } = await params;

  let filter = "";
  let snaplen = 0;
  try {
    const body = await req.json();
    filter = typeof body?.filter === "string" ? body.filter : "";
    snaplen = Number(body?.snaplen ?? 0);
  } catch {
    // No body is fine — capture everything.
  }

  if (!process.env.DISPLAY && !process.env.WAYLAND_DISPLAY) {
    return NextResponse.json(
      {
        error:
          "No graphical display detected. Start the UI from a desktop session, " +
          "or use: plasma vm capture " + id + " --wireshark",
      },
      { status: 409 },
    );
  }

  let stream;
  try {
    // Deliberately NOT tied to req.signal: this capture must outlive the
    // HTTP request that started it, for as long as Wireshark stays open.
    stream = await openCapture(id, { filter, snaplen });
  } catch (err) {
    const e = err as PlasmaError;
    return NextResponse.json({ error: e.message }, { status: e.status ?? 500 });
  }

  try {
    // -k starts capturing at once; -i - reads the pcap stream from stdin.
    const ws = spawn("wireshark", ["-k", "-i", "-"], {
      stdio: ["pipe", "ignore", "ignore"],
      detached: true,
    });

    ws.on("error", () => stream.destroy());
    // Closing Wireshark ends the capture, which stops tcpdump on the daemon.
    ws.on("exit", () => stream.destroy());
    // A broken pipe just means Wireshark went away first.
    ws.stdin.on("error", () => stream.destroy());

    stream.pipe(ws.stdin);
    ws.unref();

    return NextResponse.json({ started: true, target: id, filter });
  } catch (err) {
    stream.destroy();
    return NextResponse.json(
      { error: `could not start Wireshark: ${(err as Error).message}` },
      { status: 500 },
    );
  }
}
