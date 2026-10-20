import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/dev/txv1/page";

import { headFromMetadata } from "../../../next-compat/metadata";

export const Route = createFileRoute("/dev/txv1/")({
  component: Source,
  head: () => headFromMetadata(source.metadata),
});
