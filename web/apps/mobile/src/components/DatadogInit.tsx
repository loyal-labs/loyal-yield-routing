import { useEffect } from "react";

export function DatadogInit() {
  useEffect(() => {
    void import("@/lib/datadog/datadog").then(({ initDatadog }) => initDatadog());
  }, []);

  return null;
}
