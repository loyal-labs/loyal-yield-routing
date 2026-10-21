import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/zh/trust/page";

import { pageHead } from "../../../next-compat/metadata";

export const Route = createFileRoute("/zh/trust/")({
  component: Source,
  head: () => pageHead(source.metadata),
});
