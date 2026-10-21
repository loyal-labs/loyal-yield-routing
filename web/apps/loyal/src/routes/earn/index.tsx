import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/earn/page";

import { pageHead } from "../../next-compat/metadata";

export const Route = createFileRoute("/earn/")({
  component: Source,
  head: () => pageHead(source.metadata),
});
