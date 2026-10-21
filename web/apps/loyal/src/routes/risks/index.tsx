import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/risks/page";

import { pageHead } from "../../next-compat/metadata";

export const Route = createFileRoute("/risks/")({
  component: Source,
  head: () => pageHead(source.metadata),
});
