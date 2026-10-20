import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/trust/page";

import { headFromMetadata } from "../../next-compat/metadata";

export const Route = createFileRoute("/trust/")({
  component: Source,
  head: () => headFromMetadata(source.metadata),
});
