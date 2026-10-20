import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/zh/trust/page";

import { headFromMetadata } from "../../../next-compat/metadata";

export const Route = createFileRoute("/zh/trust/")({
  component: Source,
  head: () => headFromMetadata(source.metadata),
});
