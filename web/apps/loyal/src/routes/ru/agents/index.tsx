import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/ru/agents/page";

import { headFromMetadata } from "../../../next-compat/metadata";

export const Route = createFileRoute("/ru/agents/")({
  component: Source,
  head: () => headFromMetadata(source.metadata),
});
