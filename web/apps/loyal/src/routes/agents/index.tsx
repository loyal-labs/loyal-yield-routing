import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/agents/page";

import { headFromMetadata } from "../../next-compat/metadata";

export const Route = createFileRoute("/agents/")({
  component: Source,
  head: () => headFromMetadata(source.metadata),
});
