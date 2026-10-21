import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/zh/page";

import { pageHead } from "../../next-compat/metadata";

export const Route = createFileRoute("/zh/")({
  component: Source,
  head: () => pageHead(source.metadata),
});
