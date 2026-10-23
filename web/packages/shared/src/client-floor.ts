export const MIN_SUPPORTED_MOBILE_BUILD = 0;

export const MOBILE_BUILD_HEADER = "x-loyal-mobile-build";

export function isMobileBuildSupported(header: string | null): boolean {
	const build = header !== null && /^\d+$/.test(header) ? Number(header) : 0;
	return build >= MIN_SUPPORTED_MOBILE_BUILD;
}
