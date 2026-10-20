import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/page";

import { headFromMetadata } from "../next-compat/metadata";

export const Route = createFileRoute("/")({
  component: Source,
  head: () => headFromMetadata(source.metadata),
});
