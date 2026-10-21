import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/ru/trust/page";

import { pageHead } from "../../../next-compat/metadata";

export const Route = createFileRoute("/ru/trust/")({
  component: Source,
  head: () => pageHead(source.metadata),
});
