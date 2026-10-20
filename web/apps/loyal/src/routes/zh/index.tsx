import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/zh/page";

import { headFromMetadata } from "../../next-compat/metadata";

export const Route = createFileRoute("/zh/")({
  component: Source,
  head: () => headFromMetadata(source.metadata),
});
