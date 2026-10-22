import { getAllBlogPosts } from "@/features/blog";
import { joinLlmsHalves } from "@/lib/seo/llms-blog";
import { LLMS_FULL_HEAD, LLMS_FULL_TAIL } from "@/lib/seo/llms-content";

export const dynamic = "force-static";

export async function GET(): Promise<Response> {
  const posts = await getAllBlogPosts();
  const body = joinLlmsHalves(LLMS_FULL_HEAD, posts, LLMS_FULL_TAIL);

  return new Response(body, {
    headers: { "content-type": "text/plain; charset=utf-8" },
  });
}
