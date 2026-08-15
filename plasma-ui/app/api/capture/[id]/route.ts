import { Readable } from "node:stream";
import { NextRequest, NextResponse } from "next/server";
import { openCapture, PlasmaError } from "@/lib/plasma";

export const dynamic = "force-dynamic";

/**
 * Stream a live pcap to the browser as a download. The capture runs until the
 * client goes away: aborting the request destroys the socket to plasmad, which
 * stops tcpdump on the daemon side.
 */
export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ id: string }> },
) {
  const { id } = await params;
  const filter = req.nextUrl.searchParams.get("filter") ?? "";
  const snaplen = Number(req.nextUrl.searchParams.get("snaplen") ?? 0);

  try {
    const stream = await openCapture(id, { filter, snaplen }, req.signal);
    const stamp = new Date().toISOString().replace(/[:.]/g, "-");

    return new Response(Readable.toWeb(stream) as ReadableStream, {
      headers: {
        "Content-Type": "application/vnd.tcpdump.pcap",
        "Content-Disposition": `attachment; filename="${id}-${stamp}.pcap"`,
        "Cache-Control": "no-store",
      },
    });
  } catch (err) {
    const e = err as PlasmaError;
    return NextResponse.json({ error: e.message }, { status: e.status ?? 500 });
  }
}
