import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/ru/risks/page";

import { headFromMetadata } from "../../../next-compat/metadata";

export const Route = createFileRoute("/ru/risks/")({
  component: Source,
  head: () => headFromMetadata(source.metadata),
});
