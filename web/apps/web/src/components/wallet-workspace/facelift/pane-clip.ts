// `PANE_CLIP` contains a pane's rounded corners at every breakpoint.
// `PANE_CLIP_MOBILE_ONLY` keeps the clip on mobile only: the result screens
// use it to unclip the pane on desktop so their dog can drop into the shell's
// 8px gap and sit flush with the viewport bottom (same escape the earn empty
// pane's dog uses); the form panes keep the full clip.
export const PANE_CLIP = "overflow-clip";

export const PANE_CLIP_MOBILE_ONLY = "max-[795px]:overflow-clip";
