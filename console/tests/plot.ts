// uPlot draws on a canvas, which a document with no browser has none of: a chart is drawn by this
// stand-in in the tests, and what they read is everything around it, its legend and its table of
// numbers included. A test file takes it with vi.mock("uplot", () => import("./plot")).
export default class Plot {
  over = document.createElement("div");
  cursor: { idx: number | null } = { idx: null };
  select = { left: 0, top: 0, width: 0, height: 0 };
  constructor(_opts: unknown, _data: unknown, el: HTMLElement) {
    el.appendChild(this.over);
  }
  setData() {}
  setSize() {}
  setSelect() {}
  destroy() {}
  posToVal() {
    return 0;
  }
  valToPos() {
    return 0;
  }
  static paths = { bars: () => () => null, stepped: () => () => null };
}
