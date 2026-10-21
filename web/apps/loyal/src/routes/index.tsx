import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/page";

import { pageHead } from "../next-compat/metadata";

export const Route = createFileRoute("/")({
  component: Source,
  head: () => pageHead(source.metadata),
});
