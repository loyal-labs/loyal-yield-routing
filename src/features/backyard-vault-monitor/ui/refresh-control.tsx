"use client";

import { useRouter } from "next/navigation";
import { useEffect, useTransition } from "react";

const REFRESH_INTERVAL_MILLIS = 60_000;

export function RefreshControl({ observedAt }: { observedAt: string }) {
  const router = useRouter();
  const [isPending, startTransition] = useTransition();

  function refresh() {
    startTransition(() => router.refresh());
  }

  useEffect(() => {
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") refresh();
    }, REFRESH_INTERVAL_MILLIS);
    return () => window.clearInterval(timer);
  });

  return (
    <div className="refresh-control">
      <span className="refresh-status">
        <span className="status-dot" />
        {isPending ? "Refreshing" : "Live"} · {new Date(observedAt).toLocaleTimeString([], {
          hour: "2-digit",
          minute: "2-digit",
          second: "2-digit",
        })}
      </span>
      <button className="refresh-button" type="button" onClick={refresh} disabled={isPending}>
        {isPending ? "Refreshing…" : "Refresh"}
      </button>
    </div>
  );
}
