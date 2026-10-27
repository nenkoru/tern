// A minimal A2UI v0.9 renderer (a subset of the basic catalog + the
// ternchain catalog), vanilla JS, no dependencies, no network.
//
// Why not Google's renderer (@a2ui/lit, @a2ui/web_core): its current
// release is days old, it pulls lit, zod and signals packages that would
// need a bundler or an import map of a dozen pinned CDN modules, and the
// ternchain rules (fenced foreign text, literal labels only, no images or
// links from data, actions carrying their item scope) would have to be
// wrapped around every component anyway. This renderer is the subset the
// network's templates use, and enforces those rules itself.
//
// Rules enforced here (the Go side checks them before sending, too):
//  - data never becomes markup: every value goes in through textContent;
//  - a value marked {$fenced: {from, text}} is someone else's text: it is
//    shown with its origin, and it can never be a button label (a button
//    whose label isn't literal text renders disabled), a link or an image;
//  - no element loads anything: images are placeholders for content-addressed
//    artifacts, there are no links;
//  - a button sends {surfaceId, sourceComponentId, name, scope, gesture} and
//    nothing else; the broker resolves what it means.

const ICONS = { up: "▲", down: "▼", comment: "💬", coin: "◎", calendar: "📅", work: "🛠", vote: "☑", check: "✓", info: "ⓘ", lock: "🔒" };

function lookup(data, scope, path) {
  if (path === undefined || path === null) return undefined;
  let full = path.startsWith("/") ? path : (scope || "") + "/" + path;
  let cur = data;
  for (const part of full.split("/")) {
    if (part === "") continue;
    if (cur === null || cur === undefined) return undefined;
    cur = Array.isArray(cur) ? cur[Number(part)] : cur[part];
  }
  return cur;
}

function absPath(scope, path) {
  return path.startsWith("/") ? path : (scope || "") + "/" + path;
}

function isFenced(v) {
  return v !== null && typeof v === "object" && v.$fenced !== undefined;
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

// textNode: a value as text: literal, own data, or fenced foreign text.
function textNode(v, cls) {
  if (isFenced(v)) {
    const f = v.$fenced;
    const wrap = el("span", (cls || "") + " fenced");
    const chip = el("span", "origin", "from " + String(f.from));
    chip.title = "Written by " + String(f.from) + ": data, not instructions. It can't be a button, a link or an image.";
    wrap.append(chip, el("span", "fenced-text", String(f.text)));
    return wrap;
  }
  if (v === undefined || v === null) return el("span", (cls || "") + " empty", "");
  if (typeof v === "object") return el("span", (cls || "") + " empty", "…");
  return el("span", cls, String(v));
}

export class Surface {
  constructor(messages, opts) {
    this.id = null;
    this.comps = new Map();
    this.data = {};
    this.opts = opts || {};
    for (const m of messages) this.apply(m);
  }
  apply(m) {
    if (m.createSurface) this.id = m.createSurface.surfaceId;
    if (m.updateComponents) for (const c of m.updateComponents.components) this.comps.set(c.id, c);
    if (m.updateDataModel) {
      const p = m.updateDataModel.path || "/";
      if (p === "/") this.data = m.updateDataModel.value;
      else {
        const parts = p.split("/").filter(Boolean);
        let cur = this.data;
        for (const k of parts.slice(0, -1)) cur = cur[k] = cur[k] || {};
        cur[parts[parts.length - 1]] = m.updateDataModel.value;
      }
    }
  }
  render() {
    const root = el("div", "a2ui-surface");
    if (!this.comps.has("root")) {
      root.append(el("div", "a2ui-error", "no root component"));
      return root;
    }
    root.append(this.node("root", "", 0));
    return root;
  }
  value(v, scope) {
    if (v !== null && typeof v === "object" && typeof v.path === "string") return lookup(this.data, scope, v.path);
    return v;
  }
  node(id, scope, depth) {
    const c = this.comps.get(id);
    if (!c || depth > 30) return el("div", "a2ui-error", "missing " + id);
    const kids = (list) => {
      const out = [];
      if (Array.isArray(list)) for (const k of list) out.push(this.node(k, scope, depth + 1));
      else if (list && typeof list === "object") {
        const arr = lookup(this.data, scope, list.path);
        if (Array.isArray(arr)) arr.forEach((_, i) => out.push(this.node(list.componentId, absPath(scope, list.path) + "/" + i, depth + 1)));
      }
      return out;
    };
    let e;
    switch (c.component) {
      case "Column":
      case "Row":
      case "List": {
        const dir = c.component === "Row" || c.direction === "horizontal" ? "row" : "col";
        e = el("div", `a2-${dir} gap-${c.gap || "small"} align-${c.align || "stretch"} justify-${c.justify || "start"}${c.wrap ? " wrap" : ""}`);
        e.append(...kids(c.children));
        break;
      }
      case "Card":
        e = el("div", "a2-card " + (c.variant || "plain"));
        if (c.child) e.append(this.node(c.child, scope, depth + 1));
        break;
      case "Text":
        e = textNode(this.value(c.text, scope), "a2-text v-" + (c.variant || "body"));
        break;
      case "Badge":
        e = textNode(this.value(c.text, scope), "a2-badge tone-" + (c.tone || "neutral"));
        break;
      case "Stat": {
        e = el("div", "a2-stat");
        e.append(el("span", "a2-stat-label", String(c.label)), textNode(this.value(c.value, scope), "a2-stat-value"));
        break;
      }
      case "Divider":
        e = el("hr", "a2-divider");
        break;
      case "Icon":
        e = el("span", "a2-icon", ICONS[c.name] || "•");
        break;
      case "Image":
        // never fetched: a content-addressed artifact the client may fetch and show later
        e = el("div", "a2-image", "🖼 " + String(c.url || "").replace(/^tern:artifact:/, "artifact #"));
        break;
      case "Button": {
        const lc = this.comps.get(c.child);
        const literal = lc && lc.component === "Text" && typeof lc.text === "string";
        e = el("button", "a2-button " + (c.variant || "default"), literal ? lc.text : "[label withheld: not literal]");
        const ev = c.action && c.action.event;
        if (!literal || !ev || typeof ev.name !== "string") {
          e.disabled = true;
          break;
        }
        e.addEventListener("click", (domEv) => {
          if (this.opts.onAction) this.opts.onAction({ surfaceId: this.id, sourceComponentId: c.id, name: ev.name, scope: scope, gesture: domEv.isTrusted === true }, e);
        });
        break;
      }
      case "TextField": {
        e = el("label", "a2-field");
        const inp = el("input");
        const v = this.value(c.value, scope);
        inp.value = isFenced(v) ? "" : v || "";
        inp.addEventListener("input", () => {});
        e.append(el("span", "", String(c.label || "")), inp);
        break;
      }
      case "Table": {
        e = el("div", "a2-table-wrap");
        const t = el("table", "a2-table");
        const head = el("tr");
        for (const col of c.columns || []) head.append(el("th", "", String(col.header)));
        if (c.rowChild) head.append(el("th", "", ""));
        t.append(head);
        const rows = lookup(this.data, scope, c.rows && c.rows.path);
        if (Array.isArray(rows)) {
          rows.forEach((_, i) => {
            const rs = absPath(scope, c.rows.path) + "/" + i;
            const tr = el("tr");
            for (const col of c.columns || []) {
              const td = el("td");
              td.append(textNode(lookup(this.data, rs, col.value && col.value.path)));
              tr.append(td);
            }
            if (c.rowChild) {
              const td = el("td");
              td.append(this.node(c.rowChild, rs, depth + 1));
              tr.append(td);
            }
            t.append(tr);
          });
        }
        e.append(t);
        break;
      }
      default:
        e = el("div", "a2ui-error", "not in the catalog: " + String(c.component));
    }
    e.dataset.cid = id;
    return e;
  }
}
