import { createFileRoute } from "@tanstack/react-router";

import Source, * as source from "@/app/privacy-policy/page";

import { pageHead } from "../../next-compat/metadata";

export const Route = createFileRoute("/privacy-policy/")({
  component: Source,
  head: () => pageHead(source.metadata),
});
