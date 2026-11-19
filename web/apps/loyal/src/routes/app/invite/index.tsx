import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/app/invite/page";

import { pageHead } from "../../../next-compat/metadata";

export const Route = createFileRoute("/app/invite/")({
  component: Source,
  head: () => pageHead(source.metadata),
});
