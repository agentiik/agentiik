// What the screen tests measure on a drawn page: the console's geometry, against the rules that make
// it line up to the pixel. measure runs in the page, so it uses nothing but the DOM; each finding
// names a rule, the element by its path and its text, and what was measured.

export type Finding = { rule: string; where: string; detail: string };

export function measure(): Finding[] {
  const out: Finding[] = [];
  const rect = (el: Element) => el.getBoundingClientRect();
  const style = (el: Element) => getComputedStyle(el);
  const visible = (el: Element) => {
    const r = rect(el);
    if (r.width === 0 || r.height === 0) return false;
    const s = style(el);
    return s.visibility !== "hidden" && s.display !== "none" && Number(s.opacity) > 0;
  };
  // drawn says whether a box is seen as a box: a border above or below it, or a ground of its own.
  const drawn = (s: CSSStyleDeclaration) => parseFloat(s.borderTopWidth) > 0 || parseFloat(s.borderBottomWidth) > 0 || (s.backgroundColor !== "rgba(0, 0, 0, 0)" && s.backgroundColor !== "transparent");
  // hidden says whether an element is kept for a screen reader and not drawn.
  const hidden = (el: Element) => el.closest(".unseen") !== null || style(el).clipPath === "inset(50%)";
  const name = (el: Element) => {
    const parts: string[] = [];
    let e: Element | null = el;
    for (let i = 0; e && i < 4 && e !== document.body; i++, e = e.parentElement) {
      let p = e.tagName.toLowerCase();
      const classes = [...e.classList].filter((c) => !c.startsWith("svelte-")).slice(0, 2);
      if (classes.length) p += `.${classes.join(".")}`;
      const label = e.getAttribute("aria-label");
      if (label && i === 0) p += `[${label.slice(0, 30)}]`;
      parts.unshift(p);
    }
    const text = (el.textContent ?? "").trim().replace(/\s+/g, " ").slice(0, 40);
    return parts.join(" > ") + (text ? ` "${text}"` : "");
  };
  const all = [...document.querySelectorAll("body *")].filter(visible);

  // Nothing runs off the page sideways, unless inside a box that scrolls or clips it.
  if (document.documentElement.scrollWidth > window.innerWidth + 0.5) {
    out.push({ rule: "overflow", where: "page", detail: `${document.documentElement.scrollWidth} > ${window.innerWidth}` });
  }
  for (const el of all) {
    const r = rect(el);
    if (r.right <= window.innerWidth + 0.5 || style(el).position === "fixed") continue;
    let inside = false;
    for (let p = el.parentElement; p && !inside; p = p.parentElement) inside = ["auto", "scroll", "hidden", "clip"].includes(style(p).overflowX);
    if (!inside) out.push({ rule: "outside", where: name(el), detail: `right ${r.right.toFixed(1)} > ${window.innerWidth}` });
  }

  // Nothing on the screen runs into the page's side margins, unless inside a box that scrolls: a
  // button there sits off the column every other edge keeps, and past the window on a phone.
  const screen = document.querySelector("main.screen");
  if (screen) {
    const box = rect(screen);
    const left = box.left + parseFloat(style(screen).paddingLeft);
    const right = box.right - parseFloat(style(screen).paddingRight);
    for (const el of all) {
      // The bar across the top of a phone's screen runs the window's width on purpose.
      if (!screen.contains(el) || el === screen || hidden(el) || style(el).position === "fixed" || el.closest("[role=dialog], header.bar")) continue;
      const r = rect(el);
      if (r.right <= right + 0.5 && r.left >= left - 0.5) continue;
      let scrolls = false;
      for (let p: HTMLElement | null = el.parentElement; p && p !== screen && !scrolls; p = p.parentElement) scrolls = ["auto", "scroll"].includes(style(p).overflowX);
      if (!scrolls) out.push({ rule: "margin", where: name(el), detail: `${r.left.toFixed(1)} to ${r.right.toFixed(1)}, the page ${left.toFixed(1)} to ${right.toFixed(1)}` });
    }
  }

  // No box scrolls by a pixel or two: what overhangs a box that scrolls by so little is a box a
  // pixel too tall or too wide, and the wheel moves it.
  for (const el of all) {
    const s = style(el);
    const over = (overflow: string, by: number) => ["auto", "scroll"].includes(overflow) && by > 0 && by <= 2;
    if (over(s.overflowY, el.scrollHeight - el.clientHeight)) out.push({ rule: "scrolls", where: name(el), detail: `${el.scrollHeight - el.clientHeight}px up and down` });
    if (over(s.overflowX, el.scrollWidth - el.clientWidth)) out.push({ rule: "scrolls", where: name(el), detail: `${el.scrollWidth - el.clientWidth}px sideways` });
  }

  // Every button, field and choice is one height.
  const controlSelector = "button.control, a.control, input:not([type=checkbox]):not([type=radio]):not([type=range]), select";
  const heights = new Map<number, string[]>();
  for (const c of all.filter((e) => e.matches(controlSelector))) {
    const h = Math.round(rect(c).height * 100) / 100;
    heights.set(h, [...(heights.get(h) ?? []), name(c)]);
  }
  if (heights.size > 1) {
    for (const [h, list] of heights) out.push({ rule: "control-height", where: list.slice(0, 3).join(" | "), detail: `${h}px (${list.length})` });
  }

  // lines groups the children of a row by the line they are laid on, those whose extents overlap.
  const lines = (kids: Element[]) => {
    const found: { items: { k: Element; r: DOMRect }[]; top: number; bottom: number }[] = [];
    for (const it of kids.map((k) => ({ k, r: rect(k) })).sort((a, b) => a.r.top - b.r.top)) {
      const line = found.find((l) => it.r.top < l.bottom - 1 && it.r.bottom > l.top + 1);
      if (line) {
        line.items.push(it);
        line.top = Math.min(line.top, it.r.top);
        line.bottom = Math.max(line.bottom, it.r.bottom);
      } else found.push({ items: [it], top: it.r.top, bottom: it.r.bottom });
    }
    return found.filter((l) => l.items.length > 1);
  };
  const rows = all.filter((el) => {
    const s = style(el);
    return (s.display === "flex" || s.display === "inline-flex") && s.flexDirection.startsWith("row");
  });

  // In a row, controls share a top and a bottom edge, and the children centred share a centre.
  for (const el of rows) {
    const s = style(el);
    const kids = [...el.children].filter((k) => visible(k) && !["absolute", "fixed"].includes(style(k).position));
    for (const line of lines(kids)) {
      const controls = line.items.filter(({ k }) => k.matches(`${controlSelector}, label.select`));
      if (controls.length > 1) {
        const tops = controls.map(({ r }) => r.top);
        const bottoms = controls.map(({ r }) => r.bottom);
        const spread = Math.max(Math.max(...tops) - Math.min(...tops), Math.max(...bottoms) - Math.min(...bottoms));
        if (spread > 0.6) out.push({ rule: "row-edges", where: name(el), detail: `edges spread ${spread.toFixed(2)}px` });
      }
      const centred = line.items.filter(({ k }) => {
        const self = style(k).alignSelf;
        return (self === "auto" || self === "normal" ? s.alignItems : self) === "center";
      });
      if (centred.length > 1) {
        const centres = centred.map(({ r }) => (r.top + r.bottom) / 2);
        const spread = Math.max(...centres) - Math.min(...centres);
        if (spread > 0.6) out.push({ rule: "row-centre", where: name(el), detail: `centres spread ${spread.toFixed(2)}px` });
      }
    }
  }

  // Runs of text side by side read along one baseline. A baseline is read from a probe of no size set
  // on it just before an element's first text, in a span of its own, which a flex container takes as
  // one item as it took the text.
  const baseline = (el: Element) => {
    const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    let text = walker.nextNode();
    while (text && !text.textContent?.trim()) text = walker.nextNode();
    if (!text?.parentNode) return null;
    const wrap = document.createElement("span");
    const probe = document.createElement("span");
    probe.style.cssText = "display:inline-block;width:0;height:0;vertical-align:baseline";
    text.parentNode.insertBefore(wrap, text);
    wrap.append(probe, text);
    const y = probe.getBoundingClientRect().bottom;
    wrap.parentNode?.insertBefore(text, wrap);
    wrap.remove();
    return y;
  };
  const textual = (k: Element) => {
    const s = style(k);
    if (drawn(s) || !(k.textContent ?? "").trim() || k.querySelector("svg, img, canvas, button, input, select, .pill, .chip")) return false;
    return ["block", "inline-block", "flex", "inline-flex", "inline"].includes(s.display) && [...k.children].every((c) => !drawn(style(c)));
  };
  for (const el of rows) {
    if (style(el).alignItems === "baseline") continue;
    for (const line of lines([...el.children].filter((k) => visible(k) && textual(k)))) {
      const bases = line.items.map(({ k }) => baseline(k)).filter((v): v is number => v !== null);
      if (bases.length < 2) continue;
      const spread = Math.max(...bases) - Math.min(...bases);
      if (spread > 0.6) out.push({ rule: "baseline", where: name(el), detail: `baselines spread ${spread.toFixed(2)}px` });
    }
  }

  // A pane's title starts where each block of its content starts: a paragraph, a bar of filters, a
  // table, a list. A block starts at its first text, or at the box around it where it is in a pill,
  // a chip or a control, or at a dot, a swatch or a bullet set before it; a block drawn as a box, a
  // callout or a block of code, starts where its box does. A drawing, a graph or a chart is laid out
  // on its own and not counted, nor is a block scrolled sideways.
  const startOf = (root: Element): number | null => {
    if (drawn(style(root)) || parseFloat(style(root).borderLeftWidth) > 0) return rect(root).left;
    let first: number | null = null;
    const take = (left: number) => {
      if (first === null || left < first - 0.5) first = left;
    };
    for (const mark of root.querySelectorAll("*")) {
      if (!visible(mark) || hidden(mark) || mark.closest("svg, .canvas, .plot, .plane")) continue;
      const ms = style(mark);
      const mr = rect(mark);
      const block = ["block", "flex", "grid", "table"].includes(ms.display);
      if (drawn(ms) && (mr.width <= 40 || block) && !mark.matches("tr, td, th, tbody, thead, tr *")) take(mr.left);
      const before = getComputedStyle(mark, "::before");
      if (mark.tagName === "LI" && before.content !== "none" && before.position === "absolute") take(mr.left + (parseFloat(before.left) || 0));
    }
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const parent = n.parentElement;
      if (!n.textContent?.trim() || !parent || !visible(parent) || hidden(parent) || parent.closest("svg, .canvas, .plot, .plane, summary")) continue;
      const box = parent.closest(".pill, .chip, button, a.control, input, select, label.select, kbd");
      if (box && root.contains(box)) take(rect(box).left);
      else {
        const range = document.createRange();
        range.selectNodeContents(n);
        const r = range.getClientRects()[0];
        if (r) take(r.left);
      }
    }
    for (const box of root.querySelectorAll("input, select, textarea, button.control, a.control")) {
      if (visible(box) && !hidden(box)) take(rect(box).left);
    }
    return first;
  };
  for (const pane of all.filter((el) => el.matches("section.pane"))) {
    const h2 = pane.querySelector(":scope > header h2");
    const body = pane.querySelector(":scope > .body");
    if (!body || !visible(body)) continue;
    let edge: number | null = null;
    if (h2 && visible(h2)) {
      const range = document.createRange();
      range.selectNodeContents(h2);
      edge = range.getClientRects()[0]?.left ?? null;
    }
    for (const block of body.children) {
      if (!visible(block) || style(block).position === "absolute") continue;
      const left = startOf(block);
      if (left === null || left < rect(pane).left) continue;
      if (edge === null) edge = left;
      else if (Math.abs(left - edge) > 0.6) out.push({ rule: "pane-column", where: name(block), detail: `starts at ${left.toFixed(1)}, the pane at ${edge.toFixed(1)}` });
    }
  }

  // Every box starts on a whole pixel, and every box drawn is a whole number of pixels high. A
  // fraction is reported where it is born, on the box whose parent starts on a whole pixel, rather
  // than on every box below it that inherits it. A box turned or scaled is left out, its rectangle
  // being its outline as turned rather than where it was laid.
  const frac = (v: number) => Math.abs(v - Math.round(v)) > 0.02;
  for (const el of all) {
    const s = style(el);
    if (s.position === "fixed" || (el.closest("svg") && el.tagName !== "svg") || hidden(el)) continue;
    let moved = false;
    for (let e: Element | null = el; e && !moved; e = e.parentElement) moved = style(e).transform !== "none";
    if (moved || ((s.display === "inline" || s.display === "contents") && !drawn(s))) continue;
    const r = rect(el);
    const y = r.top + window.scrollY;
    const py = el.parentElement ? rect(el.parentElement).top + window.scrollY : 0;
    if (frac(y) && !frac(py)) out.push({ rule: "half-y", where: name(el), detail: `y=${y.toFixed(2)} in a parent at ${py.toFixed(2)}` });
    if (frac(r.height) && drawn(s) && !el.querySelector("*:not(svg *)")) out.push({ rule: "half-h", where: name(el), detail: `h=${r.height.toFixed(2)}` });
  }

  // A chart's legend that runs onto more than one row keeps its columns: every entry starts where an
  // entry of the first row does, so that a second column reads down rather than zigzag.
  for (const list of document.querySelectorAll(".chart .legend ul")) {
    const items = [...list.children].filter(visible);
    if (items.length < 2) continue;
    const top = rect(items[0]!).top;
    const columns = items.filter((li) => Math.abs(rect(li).top - top) < 0.6).map((li) => rect(li).left);
    for (const li of items) {
      const left = rect(li).left;
      if (!columns.some((c) => Math.abs(c - left) < 0.6)) out.push({ rule: "legend-column", where: name(li), detail: `starts at ${left.toFixed(1)}, the columns at ${columns.map((c) => c.toFixed(1)).join(", ")}` });
    }
  }
  return out;
}
