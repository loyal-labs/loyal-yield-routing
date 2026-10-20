import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/zh/earn/page";

import { headFromMetadata } from "../../../next-compat/metadata";

export const Route = createFileRoute("/zh/earn/")({
  component: Source,
  head: () => headFromMetadata(source.metadata),
});
