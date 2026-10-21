// The post archive half of overrides/blog-data.ts. It lives in its own module
// so the markdown stays out of the entry chunk: blog-data.ts reaches this file
// only through a dynamic import inside its async functions, so Vite keeps the
// ~145 KB of post bodies in one lazy chunk that /blog and its slugs pull and
// the other pages never download.
//
// The glob pattern walks from src/overrides/ to apps/web's public/blog, whose
// contents Vite also copies into dist verbatim (publicDir), so every post body
// parsed here and every image it references ships in the build.
import yaml from "js-yaml";
import { z } from "zod";

import type { BlogPostFull } from "@/features/blog/types";

const WORDS_PER_MINUTE = 200;
const FRONTMATTER_RE = /^---\r?\n([\s\S]*?)\r?\n---\r?\n?/;

const FrontmatterSchema = z.object({
  title: z.string().min(1),
  date: z.string().min(1),
  updated: z.string().min(1).optional(),
  hero: z.string().min(1),
  description: z.string().min(1).optional(),
  author: z
    .object({ name: z.string().min(1), avatar: z.string().min(1).optional() })
    .optional(),
  readingTime: z.number().int().positive().optional(),
});

/**
 * Every post body, read once at build time. Entries come back keyed by path
 * and in path order, i.e. slug-ascending, which is the tie the date-desc sort
 * below keeps when two posts share a date — the order Vercel's listing shows.
 */
const rawPosts = import.meta.glob<string>(
  "../../../web/public/blog/*/post.md",
  { query: "?raw", import: "default", eager: true },
);

const POST_FILENAME = "post.md";

/** Splits raw file content into parsed YAML frontmatter and the markdown body. */
function splitFrontmatter(raw: string): { data: unknown; content: string } {
  const match = raw.match(FRONTMATTER_RE);
  if (!match) {
    return { data: {}, content: raw };
  }
  // JSON_SCHEMA keeps unquoted dates (e.g. 2026-02-06) as strings instead of
  // coercing them to Date objects the way the default YAML schema would.
  const data = yaml.load(match[1], { schema: yaml.JSON_SCHEMA }) ?? {};
  return { data, content: raw.slice(match[0].length) };
}

/** Estimates reading time in minutes from the markdown body (~200 wpm). */
function estimateReadingTime(markdown: string): number {
  const words = markdown.trim().split(/\s+/).filter(Boolean).length;
  return Math.max(1, Math.round(words / WORDS_PER_MINUTE));
}

/**
 * Resolves a frontmatter media reference to a servable URL. Absolute paths and
 * full URLs pass through; bare/relative names resolve against the post folder.
 */
function resolveMedia(slug: string, ref: string): string {
  if (/^(https?:)?\/\//.test(ref) || ref.startsWith("/")) {
    return ref;
  }
  return `/blog/${slug}/${ref.replace(/^\.\//, "")}`;
}

function loadPost(file: string, raw: string): BlogPostFull | null {
  const slug = /\/blog\/([^/]+)\/post\.md$/.exec(file)?.[1];
  if (!slug) {
    return null;
  }

  const { data, content } = splitFrontmatter(raw);
  const parsed = FrontmatterSchema.safeParse(data);
  if (!parsed.success) {
    throw new Error(
      `Invalid frontmatter in blog/${slug}/${POST_FILENAME}: ${parsed.error.message}`
    );
  }

  const fm = parsed.data;
  return {
    slug,
    title: fm.title,
    date: fm.date,
    updated: fm.updated,
    hero: resolveMedia(slug, fm.hero),
    description: fm.description,
    author: fm.author
      ? {
          name: fm.author.name,
          avatar: fm.author.avatar
            ? resolveMedia(slug, fm.author.avatar)
            : undefined,
        }
      : undefined,
    readingTime: fm.readingTime ?? estimateReadingTime(content),
    content,
  };
}

export const allPosts: BlogPostFull[] = Object.entries(rawPosts)
  .map(([file, raw]) => loadPost(file, raw))
  .filter((post): post is BlogPostFull => post !== null)
  .sort((a, b) => b.date.localeCompare(a.date));
