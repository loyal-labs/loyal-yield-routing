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

// Next's fill layout: the image is taken out of flow and stretched over its
// (positioned) parent, keeping an object-fit the caller may set in style.
export function fillStyle(style?: CSSProperties): CSSProperties {
  return {
    ...style,
    position: "absolute",
    inset: 0,
    width: "100%",
    height: "100%",
    objectFit: style?.objectFit ?? "cover",
  };
}

// quality/unoptimized/placeholder and the /_next/image optimizer have no
// equivalent here: src is served as authored, from public/ or the bundle.
// With fill the width/height attributes stay off, as in Next; the CSS owns
// the size.
export default function Image({ src, fill, priority, quality: _quality, unoptimized: _unoptimized, placeholder: _placeholder, width, height, style, ...rest }: ImageProps) {
  const staticImage = typeof src === "string" ? undefined : src;
  return (
    <img
      {...rest}
      loading={priority ? "eager" : "lazy"}
      fetchPriority={priority ? "high" : undefined}
      style={fill ? fillStyle(style) : style}
      src={typeof src === "string" ? src : src.src}
      width={fill ? undefined : (width ?? staticImage?.width)}
      height={fill ? undefined : (height ?? staticImage?.height)}
    />
  );
}
