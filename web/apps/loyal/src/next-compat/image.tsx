import type { CSSProperties, ImgHTMLAttributes } from "react";

export type StaticImageData = { src: string; width: number; height: number; blurDataURL?: string };

export type ImageProps = Omit<ImgHTMLAttributes<HTMLImageElement>, "src" | "width" | "height"> & {
  src: string | StaticImageData;
  alt: string;
  width?: number | `${number}`;
  height?: number | `${number}`;
  fill?: boolean;
  priority?: boolean;
  quality?: number;
  sizes?: string;
  unoptimized?: boolean;
  placeholder?: string;
  style?: CSSProperties;
};

// Seed body: a plain img. G1 handles fill, priority and static imports fully.
export default function Image({ src, fill: _fill, priority: _priority, quality: _quality, unoptimized: _unoptimized, placeholder: _placeholder, ...rest }: ImageProps) {
  return <img src={typeof src === "string" ? src : src.src} {...rest} />;
}
