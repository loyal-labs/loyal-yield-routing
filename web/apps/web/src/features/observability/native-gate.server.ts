import "server-only";

export function isNativeAppRequest(request: Request): boolean {
  return (
    !request.headers.get("origin") && !request.headers.get("sec-fetch-site")
  );
}
