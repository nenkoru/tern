// tern-testnet-1 site: language, live status (same-origin JSON-RPC), downloads from SHA256SUMS, copy buttons.
// No cookies, no third-party requests; the language choice is kept in localStorage when allowed.
(function () {
  "use strict";
  var CHAIN = 83760;
  var root = document.documentElement;

  // ---- language
  function getLang() {
    var q = new URLSearchParams(location.search).get("lang");
    if (q === "ru" || q === "en") return q;
    try { var s = localStorage.getItem("tern-lang"); if (s === "ru" || s === "en") return s; } catch (e) {}
    return /^ru|^uk|^be|^kk/i.test(navigator.language || "") ? "ru" : "en";
  }
  function setLang(l) {
    root.setAttribute("data-lang", l);
    root.setAttribute("lang", l);
    document.querySelectorAll(".langs button").forEach(function (b) { b.setAttribute("aria-pressed", String(b.dataset.set === l)); });
    try { localStorage.setItem("tern-lang", l); } catch (e) {}
    document.title = l === "ru" ? "tern — тестовая сеть агентов" : "tern — a test network for agents";
    renderStatus();
  }
  document.querySelectorAll(".langs button").forEach(function (b) { b.addEventListener("click", function () { setLang(b.dataset.set); }); });

  // ---- copy buttons on code blocks
  document.querySelectorAll("pre").forEach(function (pre) {
    var box = document.createElement("div"); box.className = "code";
    pre.parentNode.insertBefore(box, pre); box.appendChild(pre);
    var b = document.createElement("button"); b.className = "copy"; b.type = "button";
    b.textContent = "copy";
    b.addEventListener("click", function () {
      var t = pre.innerText;
      (navigator.clipboard ? navigator.clipboard.writeText(t) : Promise.reject()).then(function () {
        b.textContent = root.getAttribute("data-lang") === "ru" ? "скопировано" : "copied";
        setTimeout(function () { b.textContent = "copy"; }, 1500);
      }).catch(function () { b.textContent = "select + ⌘C"; });
    });
    box.appendChild(b);
  });

  // ---- live status
  var st = { ok: null, chain: null, head: null, ts: null, vals: null, err: "" };
  function rpc(method, params) {
    return fetch("/", { method: "POST", headers: { "Content-Type": "application/json" }, cache: "no-store",
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: method, params: params || [] }) })
      .then(function (r) { return r.json(); })
      .then(function (d) { if (d.error) throw new Error(d.error.message); return d.result; });
  }
  function ago(sec, l) {
    if (sec < 0) sec = 0;
    if (sec < 90) return l === "ru" ? sec + " с назад" : sec + " s ago";
    var m = Math.round(sec / 60);
    if (m < 90) return l === "ru" ? m + " мин назад" : m + " min ago";
    var h = Math.round(m / 60);
    return l === "ru" ? h + " ч назад" : h + " h ago";
  }
  function text(id, v) { var e = document.getElementById(id); if (e) e.textContent = v; }
  function renderStatus() {
    var l = root.getAttribute("data-lang") || "en";
    var dot = document.getElementById("st-dot");
    if (!dot) return;
    var now = Math.floor(Date.now() / 1000);
    var fresh = st.ts !== null && now - st.ts < 30;
    var right = st.chain === CHAIN;
    dot.className = "dot " + (st.ok === null ? "" : st.ok && right && fresh ? "ok" : "bad");
    var label;
    if (st.ok === null) label = l === "ru" ? "проверяю сеть…" : "checking the network…";
    else if (!st.ok) label = l === "ru" ? "сеть не отвечает" : "the network does not answer";
    else if (!right) label = l === "ru" ? "по этому адресу пока работает сезон-девнет (chain " + st.chain + "): tern-testnet-1 ещё не запущен"
      : "this address still serves the season devnet (chain " + st.chain + "): tern-testnet-1 is not live yet";
    else if (!fresh) label = l === "ru" ? "блоки не идут" : "no new blocks";
    else label = l === "ru" ? "tern-testnet-1 работает" : "tern-testnet-1 is live";
    text("st-label", label);
    text("st-chain", st.chain === null ? "—" : String(st.chain));
    text("st-head", st.head === null ? "—" : "#" + st.head.toLocaleString(l === "ru" ? "ru-RU" : "en-US"));
    text("st-last", st.ts === null ? "—" : ago(now - st.ts, l));
    text("st-vals", st.vals === null ? "—" : String(st.vals));
  }
  function poll() {
    Promise.all([rpc("eth_chainId"), rpc("eth_getBlockByNumber", ["latest", false])])
      .then(function (r) {
        st.ok = true; st.chain = parseInt(r[0], 16);
        st.head = parseInt(r[1].number, 16); st.ts = parseInt(r[1].timestamp, 16);
        return rpc("qbft_getValidatorsByBlockNumber", ["latest"]).then(function (v) { st.vals = v.length; }, function () { st.vals = null; });
      })
      .catch(function () { st.ok = false; })
      .then(renderStatus);
  }
  setInterval(renderStatus, 1000);
  setInterval(function () { if (!document.hidden) poll(); }, 5000);

  // ---- downloads from SHA256SUMS
  var LABEL = {
    "darwin-arm64": ["Mac (Apple silicon, M1–M4)", "Mac (Apple silicon, M1–M4)"],
    "darwin-amd64": ["Mac (Intel)", "Mac (Intel)"],
    "linux-amd64": ["Linux (x86-64)", "Linux (x86-64)"],
    "linux-arm64": ["Linux (ARM64)", "Linux (ARM64)"],
    "windows-amd64.exe": ["Windows (x86-64)", "Windows (x86-64)"],
    "windows-arm64.exe": ["Windows (ARM64)", "Windows (ARM64)"]
  };
  function mine() {
    var ua = navigator.userAgent || "";
    if (/Windows/.test(ua)) return "windows-amd64.exe";
    if (/Mac OS X|Macintosh/.test(ua)) return "darwin-arm64";
    if (/Linux/.test(ua) && !/Android/.test(ua)) return "linux-amd64";
    return "";
  }
  fetch("SHA256SUMS", { cache: "no-store" }).then(function (r) { if (!r.ok) throw 0; return r.text(); }).then(function (t) {
    var rows = t.trim().split("\n").map(function (x) { var p = x.trim().split(/\s+/); return { sum: p[0], name: p[1] }; })
      .filter(function (x) { return x.name && /^ternchain-/.test(x.name); });
    if (!rows.length) return;
    var guess = mine();
    document.querySelectorAll(".dl[data-files]").forEach(function (box) {
      var ru = box.getAttribute("data-l") === "ru";
      box.innerHTML = "";
      rows.forEach(function (x) {
        var key = Object.keys(LABEL).filter(function (k) { return x.name.slice(-k.length) === k; })[0];
        var a = document.createElement("a"); a.className = "file" + (key && key === guess ? " mine" : ""); a.href = x.name; a.setAttribute("download", "");
        var b = document.createElement("b"); b.textContent = (key ? LABEL[key][ru ? 1 : 0] : x.name) + (key && key === guess ? (ru ? " — похоже, ваш" : " — likely yours") : "");
        var s = document.createElement("span"); s.textContent = x.name + " · sha256 " + x.sum.slice(0, 16) + "…";
        a.appendChild(b); a.appendChild(s); box.appendChild(a);
      });
    });
  }).catch(function () {});

  setLang(getLang());
  poll();
})();
