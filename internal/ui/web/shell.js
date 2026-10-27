// The chat-first shell: one stream with your agent. Agent messages carry
// A2UI surfaces (a feed, a thread, a card); buttons go to the broker as
// typed events; approvals are drawn here, in the trusted layer, from the
// structured action the server returns, never by a surface.
import { Surface } from "./a2ui.js";

const token = document.querySelector('meta[name="tern-shell"]').content;
const chat = document.getElementById("chat");
const form = document.getElementById("say");
const input = document.getElementById("say-text");
const params = new URLSearchParams(location.search);
const onlyLast = params.has("only") ? Number(params.get("only")) || 1 : 0;
let seen = new Set();

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

async function post(path, body) {
  const r = await fetch(path, { method: "POST", headers: { "content-type": "application/json", "x-tern-shell": token }, body: JSON.stringify(body) });
  return r.json();
}

function toast(text, tone) {
  const t = el("div", "toast " + (tone || ""), text);
  document.body.append(t);
  setTimeout(() => t.remove(), 4200);
}

function renderMessage(m) {
  const row = el("div", "msg " + m.from);
  row.dataset.mid = m.id;
  const who = el("div", "who", m.from === "owner" ? "you" : m.from === "agent" ? m.agent || "agent" : "system");
  const bubble = el("div", "bubble");
  if (m.text) for (const line of String(m.text).split("\n")) bubble.append(el("p", "", line));
  if (m.surface) {
    const s = m.surface;
    const box = el("div", "surface-box" + (s.tainted ? " tainted" : ""));
    if (s.theme) for (const [k, v] of Object.entries(s.theme)) if (/^--[a-z-]+$/.test(k)) box.style.setProperty(k, v);
    const surf = new Surface(s.a2ui, { onAction: (ev, btn) => act(ev, btn) });
    box.append(surf.render());
    const foot = el("div", "surface-foot");
    foot.append(el("span", "src", "drawn by: " + s.source));
    if (s.note) foot.append(el("span", "note", s.note));
    if (s.tainted) foot.append(el("span", "taint", "shows others' text (fenced): its buttons are treated as from a tainted agent"));
    box.append(foot);
    if (s.drafts && s.drafts.length) {
      const d = el("details", "drafts");
      d.append(el("summary", "", `how this screen was made: ${s.drafts.length} draft(s), checked before showing`));
      for (const dr of s.drafts) {
        const p = el("div", "draft " + (dr.errors && dr.errors.length ? "bad" : "ok"));
        p.append(el("div", "draft-h", `draft ${dr.attempt} (~${dr.tokens} tokens): ${dr.errors && dr.errors.length ? "rejected" : "valid"}`));
        if (dr.errors) for (const er of dr.errors) p.append(el("div", "draft-e", er));
        p.append(el("pre", "", dr.openui));
        d.append(p);
      }
      box.append(d);
    }
    bubble.append(box);
  }
  row.append(who, bubble);
  return row;
}

function add(m) {
  if (seen.has(m.id)) return;
  seen.add(m.id);
  chat.append(renderMessage(m));
  if (onlyLast) {
    const rows = [...chat.querySelectorAll(".msg")];
    rows.slice(0, Math.max(0, rows.length - onlyLast)).forEach((r) => (r.style.display = "none"));
  }
  window.scrollTo(0, document.body.scrollHeight);
}

async function act(ev, btn) {
  btn.disabled = true;
  try {
    const r = await post("/api/action", ev);
    if (r.status === "needs_approval" && r.ask) openSheet(r.ask);
    else if (r.status === "done") toast("done: " + r.message, "good");
    else if (r.status === "local") {}
    else toast(r.status + ": " + r.message, "bad");
  } finally {
    btn.disabled = false;
  }
}

// ---- the trusted layer: the owner's approval sheet

let mark = null;
async function ownerMark() {
  if (mark) return mark;
  const r = await fetch("/trusted/mark", { headers: { "x-tern-shell": token } });
  mark = await r.json();
  return mark;
}

async function openSheet(ask) {
  const layer = document.getElementById("trusted");
  layer.replaceChildren();
  const m = await ownerMark();
  const sheet = el("div", "sheet");
  const head = el("div", "sheet-head");
  const sw = el("span", "mark-swatch");
  sw.style.background = m.color || "#888";
  head.append(sw, el("span", "mark-word", m.word || "(no mark set)"), el("span", "sheet-title", "Your approval — drawn by your agent's shell, not by the screen"));
  sheet.append(head);
  const rows = [
    ["Action", ask.what],
    ["Kind", ask.action.kind],
    ["To", ask.counterparty + (ask.rating ? "  ·  " + ask.rating : "")],
  ];
  if (ask.amount) rows.push(["Amount", ask.amount + " coins"]);
  rows.push(["Screen", ask.action.source + (ask.action.tainted ? " (shows others' text)" : "")]);
  rows.push(["Action hash", ask.action.action_hash]);
  const tbl = el("table", "sheet-rows");
  for (const [k, v] of rows) {
    const tr = el("tr");
    tr.append(el("th", "", k), el("td", "", v));
    tbl.append(tr);
  }
  sheet.append(tbl);
  const why = el("div", "sheet-why");
  why.append(el("div", "sheet-why-h", "Why you see this"));
  for (const w of ask.why || []) why.append(el("div", "", "· " + w));
  sheet.append(why);
  const cmd = el("div", "sheet-cmd");
  cmd.append(el("div", "", "Or from your own machine:"), el("code", "", ask.command));
  sheet.append(cmd);
  const btns = el("div", "sheet-btns");
  const yes = el("button", "approve", "Approve exactly this");
  const no = el("button", "decline", "Decline");
  yes.disabled = true;
  setTimeout(() => (yes.disabled = false), 600); // no click-through: a moment to read
  yes.addEventListener("click", async (e) => {
    if (!e.isTrusted) return;
    yes.disabled = true;
    yes.textContent = "Signing your approval…";
    const r = await post("/trusted/approve", { id: ask.id });
    layer.replaceChildren();
    toast((r.status === "done" ? "done: " : r.status + ": ") + r.message, r.status === "done" ? "good" : "bad");
  });
  no.addEventListener("click", async () => {
    await post("/trusted/decline", { id: ask.id });
    layer.replaceChildren();
  });
  btns.append(yes, no);
  sheet.append(btns);
  sheet.append(el("div", "sheet-foot", "A real approval always shows your secret mark. A sheet without it, or inside the chat, is fake."));
  layer.append(sheet);
}

// ---- the stream

function connect() {
  const es = new EventSource("/api/events?t=" + encodeURIComponent(token));
  es.addEventListener("message", (e) => add(JSON.parse(e.data)));
  es.addEventListener("state", (e) => {
    const s = JSON.parse(e.data);
    document.getElementById("who").textContent = s.agent + (s.owner ? " · agent of " + s.owner : "");
    document.getElementById("state").textContent = `community ${s.community} · UI ${s.ui} · ranking ${s.ranker}`;
  });
  es.onerror = () => {};
}

form.addEventListener("submit", async (e) => {
  e.preventDefault();
  const text = input.value.trim();
  if (!text) return;
  input.value = "";
  await post("/api/say", { text });
});

for (const b of document.querySelectorAll("[data-say]")) b.addEventListener("click", () => post("/api/say", { text: b.dataset.say }));

connect();
if (params.get("say")) {
  setTimeout(() => post("/api/say", { text: params.get("say") }), 300);
}
