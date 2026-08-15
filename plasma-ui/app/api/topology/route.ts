import { NextResponse } from "next/server";
import { getTopology, PlasmaError } from "@/lib/plasma";

// Live fleet state — never cache.
export const dynamic = "force-dynamic";

export async function GET() {
  try {
    return NextResponse.json(await getTopology());
  } catch (err) {
    const e = err as PlasmaError;
    return NextResponse.json(
      { error: e.message },
      { status: e.status ?? 500 },
    );
  }
}
