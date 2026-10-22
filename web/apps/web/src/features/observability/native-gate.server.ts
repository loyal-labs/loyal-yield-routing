import "server-only";

// Native fetch never sends Origin or Sec-Fetch-Site, while browsers always send
// them on cross-site POSTs, so their presence marks traffic these routes are not for.
export function isNativeAppRequest(request: Request): boolean {
  return (
    !request.headers.get("origin") && !request.headers.get("sec-fetch-site")
  );
}
