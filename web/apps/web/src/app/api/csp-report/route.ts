import { reportServerError } from "@/features/observability/server";

export const runtime = "nodejs";

export async function POST(request: Request) {
  try {
    const body = (await request.json().catch(() => null)) as {
      "csp-report"?: Record<string, unknown>;
    } | null;
    const report = body?.["csp-report"];
    if (report) {
      const blocked = String(report["blocked-uri"] ?? "");
      const directive = String(
        report["effective-directive"] ?? report["violated-directive"] ?? ""
      );
      const doc = String(report["document-uri"] ?? "");
      await reportServerError(
        new Error(`CSP ${directive} blocked ${blocked} on ${doc}`),
        {
          method: "POST",
          operation: "next.request.error",
          pathname: "/api/csp-report",
          severity: "WARN",
        }
      );
    }
  } catch {
  }
  return new Response(null, { status: 204 });
}
