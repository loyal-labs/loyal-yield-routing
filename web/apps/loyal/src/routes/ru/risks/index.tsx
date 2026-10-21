import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/ru/risks/page";

import { pageHead } from "../../../next-compat/metadata";

export const Route = createFileRoute("/ru/risks/")({
  component: Source,
  head: () => pageHead(source.metadata),
});
