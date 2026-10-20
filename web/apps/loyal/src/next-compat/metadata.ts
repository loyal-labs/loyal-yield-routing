import type { Metadata } from "next";

export type Head = {
  meta: Array<Record<string, string>>;
  links: Array<Record<string, string>>;
};

// Seed body: title and description. G1 maps openGraph, twitter, alternates
// and icons so a route's head equals what Next renders for the same metadata.
export function headFromMetadata(metadata: Metadata | undefined): Head {
  const meta: Head["meta"] = [];
  if (typeof metadata?.title === "string") meta.push({ title: metadata.title });
  if (metadata?.description) meta.push({ name: "description", content: metadata.description });
  return { meta, links: [] };
}
