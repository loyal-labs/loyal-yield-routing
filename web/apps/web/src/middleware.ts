import { NextResponse, type NextRequest } from "next/server";

import { handleLocaleRedirect } from "@/features/marketing/i18n/locale-redirect";
import { rejectUnsupportedMobileBuild } from "@/lib/mobile-build-floor.server";

const appHostnames = new Set(["app.askloyal.com"]);

const mobileEarnApiPrefix = "/api/smart-accounts/mobile/earn/";

export function middleware(request: NextRequest) {
  if (request.nextUrl.pathname.startsWith(mobileEarnApiPrefix)) {
    return rejectUnsupportedMobileBuild(request) ?? NextResponse.next();
  }

  const hostname = request.headers.get("host")?.split(":")[0] ?? "";

  if (!appHostnames.has(hostname)) {
    return handleLocaleRedirect(request) ?? NextResponse.next();
  }

  const url = request.nextUrl.clone();

  if (url.pathname === "/") {
    url.pathname = "/app";
    return NextResponse.rewrite(url);
  }

  return NextResponse.next();
}

export const config = {
  matcher: [
    "/((?!_next|api|favicon.ico|.*\\..*).*)",
    "/api/smart-accounts/mobile/earn/:path*",
  ],
};
