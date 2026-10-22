import { getAllBlogPosts } from "@/features/blog";
import { renderBlogSection } from "@/lib/seo/llms-blog";
import { LLMS_FULL_HEAD, LLMS_FULL_TAIL } from "@/lib/seo/llms-content";

export const dynamic = "force-static";

export async function GET(): Promise<Response> {
  const posts = await getAllBlogPosts();
  // The curated halves already carry their own blank-line spacing, so the
  // section is joined without adding another leading newline.
  const body = `${LLMS_FULL_HEAD}${renderBlogSection(posts)}\n${LLMS_FULL_TAIL}`;

  return new Response(body, {
    headers: { "content-type": "text/plain; charset=utf-8" },
  });
}
