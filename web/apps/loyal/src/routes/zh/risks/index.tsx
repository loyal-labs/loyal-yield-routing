import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/zh/risks/page";

import { pageHead } from "../../../next-compat/metadata";

export const Route = createFileRoute("/zh/risks/")({
  component: Source,
  head: () => pageHead(source.metadata),
});
