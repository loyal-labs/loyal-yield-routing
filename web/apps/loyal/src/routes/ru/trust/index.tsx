import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/ru/trust/page";

import { headFromMetadata } from "../../../next-compat/metadata";

export const Route = createFileRoute("/ru/trust/")({
  component: Source,
  head: () => headFromMetadata(source.metadata),
});
