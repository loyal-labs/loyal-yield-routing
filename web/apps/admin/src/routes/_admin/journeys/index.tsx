import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/journeys/page";

import type { ComponentProps } from "react";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/journeys", typeof Source>;

export const propsMatchView: Props extends ComponentProps<typeof Source> ? true : never = true;

export const Route = createFileRoute("/_admin/journeys/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
