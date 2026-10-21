import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/ru/page";

import { pageHead } from "../../next-compat/metadata";

export const Route = createFileRoute("/ru/")({
  component: Source,
  head: () => pageHead(source.metadata),
});
