import { type ComponentType, lazy, type ReactNode, Suspense } from "react";

type Loaded<P> = ComponentType<P> | { default: ComponentType<P> };

export type DynamicOptions = { ssr?: boolean; loading?: () => ReactNode };

export default function dynamic<P extends object>(load: () => Promise<Loaded<P>>, options: DynamicOptions = {}): ComponentType<P> {
  const Lazy = lazy(async () => {
    const loaded = await load();
    return { default: "default" in loaded ? loaded.default : loaded };
  });
  return function Dynamic(props: P) {
    return (
      <Suspense fallback={options.loading?.() ?? null}>
        <Lazy {...props} />
      </Suspense>
    );
  };
}
