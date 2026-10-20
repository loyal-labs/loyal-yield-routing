import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/ru/earn/page";

import { headFromMetadata } from "../../../next-compat/metadata";

export const Route = createFileRoute("/ru/earn/")({
  component: Source,
  head: () => headFromMetadata(source.metadata),
});
