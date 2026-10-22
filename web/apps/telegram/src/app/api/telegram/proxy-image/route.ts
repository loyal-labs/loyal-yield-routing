import { NextRequest, NextResponse } from "next/server";

export async function GET(req: NextRequest) {
  const searchParams = req.nextUrl.searchParams;
  const url = searchParams.get("url");

  if (!url) {
    return NextResponse.json(
      { error: "Missing url parameter" },
      { status: 400 }
    );
  }

  try {
    const targetUrl = new URL(url);
    const allowedHosts = [
      "t.me",
      "cdn4.telesco.pe",
      "cdn1.telesco.pe",
      "cdn2.telesco.pe",
      "cdn3.telesco.pe",
      "cdn5.telesco.pe",
    ];

    if (
      targetUrl.protocol !== "https:" ||
      !allowedHosts.some(
        (host) =>
          targetUrl.hostname === host || targetUrl.hostname.endsWith(`.${host}`)
      )
    ) {
      return NextResponse.json({ error: "Invalid host" }, { status: 400 });
    }

    const response = await fetch(targetUrl, { redirect: "manual" });

    if (response.status >= 300 && response.status < 400) {
      return NextResponse.json(
        { error: "Redirects are not followed" },
        { status: 502 }
      );
    }

    if (!response.ok) {
      return NextResponse.json(
        { error: `Failed to fetch image: ${response.statusText}` },
        { status: response.status }
      );
    }

    const contentType = response.headers.get("content-type") ?? "";
    if (
      !contentType.startsWith("image/") ||
      contentType.startsWith("image/svg+xml")
    ) {
      return NextResponse.json(
        { error: "Upstream is not an image" },
        { status: 415 }
      );
    }
    const buffer = await response.arrayBuffer();

    return new NextResponse(buffer, {
      headers: {
        "Content-Type": contentType,
        "Cache-Control": "public, max-age=86400",
      },
    });
  } catch (error) {
    console.error("Proxy error:", error);
    return NextResponse.json(
      { error: "Internal server error" },
      { status: 500 }
    );
  }
}
