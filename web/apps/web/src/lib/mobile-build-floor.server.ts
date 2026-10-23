import { isMobileBuildSupported, MOBILE_BUILD_HEADER } from "@loyal-labs/shared";
import { NextResponse } from "next/server";

export function rejectUnsupportedMobileBuild(
  request: Request
): NextResponse | null {
  if (isMobileBuildSupported(request.headers.get(MOBILE_BUILD_HEADER))) {
    return null;
  }
  return NextResponse.json(
    {
      error: {
        code: "upgrade_required",
        message: "Update the Loyal app to continue.",
      },
    },
    { status: 426 }
  );
}
