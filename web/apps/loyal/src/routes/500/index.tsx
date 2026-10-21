import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/500/page";

import { pageHead } from "../../next-compat/metadata";

export const Route = createFileRoute("/500/")({
  component: Source,
  head: () => pageHead({ robots: { index: false } }),
});
