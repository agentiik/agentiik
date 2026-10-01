// The design system's icons, vendored from agentiik/design, each a 16 by 16 drawing in currentColor so
// that it takes the colour of the text beside it.

const drawings = import.meta.glob("../../vendor/icons/*.svg", { query: "?raw", import: "default", eager: true }) as Record<string, string>;

export const icons: Record<string, string> = Object.fromEntries(
  Object.entries(drawings).map(([path, svg]) => [path.replace(/^.*\/(.*)\.svg$/, "$1"), svg.replace(/ width="16" height="16"/, "")]),
);
