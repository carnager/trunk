// trunk's web client: the library in a browser. Plain JavaScript, no build
// step, nothing loaded from elsewhere. It talks to the API like the phone app;
// pictures load with the token in a cookie, which trunk accepts for reading.

// ---------- words ----------

const de = navigator.language.toLowerCase().startsWith("de");
const words = {
  photos: ["Photos", "Fotos"], albums: ["Albums", "Alben"], people: ["People", "Personen"], favorites: ["Favorites", "Favoriten"],
  trash: ["Trash", "Papierkorb"], search: ["Search: place, date, name, caption …", "Suchen: Ort, Datum, Name, Beschreibung …"],
  user: ["Name", "Name"], password: ["Password", "Passwort"], login: ["Sign in", "Anmelden"], logout: ["Sign out", "Abmelden"],
  wrong: ["Name or password wrong", "Name oder Passwort falsch"], loading: ["Loading the library …", "Lade die Bibliothek …"],
  nothing: ["Nothing here yet", "Hier ist noch nichts"], found: ["Nothing found", "Nichts gefunden"],
  today: ["Today", "Heute"], yesterday: ["Yesterday", "Gestern"], selected: ["selected", "ausgewählt"],
  favorite: ["Favorite", "Favorit"], unfavorite: ["No favorite", "Kein Favorit"], album: ["Album", "Album"], delete: ["Delete", "Löschen"],
  restore: ["Restore", "Wiederherstellen"], deleteForever: ["Delete for good", "Endgültig löschen"], download: ["Download", "Herunterladen"],
  upload: ["Upload", "Hochladen"], uploading: ["Uploading", "Lade hoch"], uploaded: ["uploaded", "hochgeladen"], dropHere: ["Drop to upload to trunk", "Loslassen zum Hochladen"],
  trashed: ["Moved to trash", "In den Papierkorb gelegt"], undo: ["Undo", "Rückgängig"], addTo: ["Add to album", "Zu Album hinzufügen"],
  newAlbum: ["New album …", "Neues Album …"], albumName: ["Album name", "Name des Albums"], create: ["Create", "Anlegen"], cancel: ["Cancel", "Abbrechen"],
  added: ["Added", "Hinzugefügt"], details: ["Details", "Details"], taken: ["Taken", "Aufgenommen"], place: ["Place", "Ort"], camera: ["Camera", "Kamera"],
  caption: ["Caption", "Beschreibung"], captionHint: ["Add a caption", "Beschreibung hinzufügen"], file: ["File", "Datei"], inPhoto: ["In this photo", "Auf diesem Bild"],
  nameIt: ["Name?", "Name?"], unnamed: ["Without a name", "Ohne Namen"], typeName: ["Type a name", "Namen eingeben"], motion: ["Play motion", "Bewegung abspielen"],
  photo1: ["photo", "Bild"], photoN: ["photos", "Bilder"], tooBig: ["larger than this trunk accepts", "größer als dieser trunk annimmt"],
  emptyTrash: ["Photos you delete stay here for a while.", "Gelöschte Bilder bleiben hier eine Weile."], sharedBy: ["shared by", "geteilt von"],
};
const t = (k) => (words[k] || [k, k])[de ? 1 : 0];
const count = (n) => `${n} ${n === 1 ? t("photo1") : t("photoN")}`;
const locale = de ? "de-DE" : navigator.language;

// ---------- icons (Material Symbols paths) ----------

const icons = {
  search: "M15.5 14h-.79l-.28-.27A6.47 6.47 0 0 0 16 9.5 6.5 6.5 0 1 0 9.5 16c1.61 0 3.09-.59 4.23-1.57l.27.28v.79l5 4.99L20.49 19zm-6 0C7.01 14 5 11.99 5 9.5S7.01 5 9.5 5 14 7.01 14 9.5 11.99 14 9.5 14z",
  upload: "M5 20h14v-2H5zm0-10h4v6h6v-6h4l-7-7z",
  logout: "M17 7l-1.41 1.41L18.17 11H8v2h10.17l-2.58 2.58L17 17l5-5zM4 5h8V3H4c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h8v-2H4z",
  close: "M19 6.41 17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z",
  prev: "M15.41 7.41 14 6l-6 6 6 6 1.41-1.41L10.83 12z",
  next: "M10 6 8.59 7.41 13.17 12l-4.58 4.59L10 18l6-6z",
  heart: "M12 21.35l-1.45-1.32C5.4 15.36 2 12.28 2 8.5 2 5.42 4.42 3 7.5 3c1.74 0 3.41.81 4.5 2.09C13.09 3.81 14.76 3 16.5 3 19.58 3 22 5.42 22 8.5c0 3.78-3.4 6.86-8.55 11.54z",
  info: "M11 7h2v2h-2zm0 4h2v6h-2zm1-9C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm0 18c-4.41 0-8-3.59-8-8s3.59-8 8-8 8 3.59 8 8-3.59 8-8 8z",
  trash: "M6 19c0 1.1.9 2 2 2h8c1.1 0 2-.9 2-2V7H6zM19 4h-3.5l-1-1h-5l-1 1H5v2h14z",
  download: "M19 9h-4V3H9v6H5l7 7zM5 18v2h14v-2z",
  play: "M8 5v14l11-7z",
  motion: "M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm-2 14.5v-9l6 4.5z",
  album: "M18 2H6c-1.1 0-2 .9-2 2v16c0 1.1.9 2 2 2h12c1.1 0 2-.9 2-2V4c0-1.1-.9-2-2-2zM6 4h5v8l-2.5-1.5L6 12z",
  restore: "M13 3a9 9 0 0 0-9 9H1l3.89 3.89.07.14L9 12H6c0-3.87 3.13-7 7-7s7 3.13 7 7-3.13 7-7 7c-1.93 0-3.68-.79-4.94-2.06l-1.42 1.42A8.954 8.954 0 0 0 13 21a9 9 0 0 0 0-18z",
};
const svg = (name, cls = "") => `<svg viewBox="0 0 24 24" fill="currentColor" class="${cls}" aria-hidden="true"><path d="${icons[name]}"/></svg>`;
const mark = `<svg viewBox="17 24 74 52" aria-hidden="true"><circle cx="34" cy="43" r="14" fill="#4429B8"/><circle cx="74" cy="43" r="14" fill="#4429B8"/><path d="M36,48 C36,35 44,28 54,28 C64,28 72,35 72,48 C72,57 64,61 54,61 C44,61 36,57 36,48 Z" fill="#5B3FD9"/><ellipse cx="47.8" cy="43" rx="4.3" ry="5.2" fill="#fff"/><circle cx="49.1" cy="44.6" r="2.6" fill="#1E1B3A"/><ellipse cx="60.2" cy="43" rx="4.3" ry="5.2" fill="#fff"/><circle cx="61.5" cy="44.6" r="2.6" fill="#1E1B3A"/><path d="M54,56 C54.5,62 54,65.5 52.3,68 C50.6,70.5 47,71.5 45.4,69.6" fill="none" stroke="#5B3FD9" stroke-width="6.4" stroke-linecap="round"/></svg>`;

// ---------- small helpers ----------

const $ = (sel, root = document) => root.querySelector(sel);
function h(tag, attrs = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "html") el.innerHTML = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) if (kid != null && kid !== false) el.append(kid);
  return el;
}
// replaceChildren, leaving out what is not there (null, false) instead of writing "null".
const fill = (el, ...kids) => el.replaceChildren(...kids.flat().filter((k) => k != null && k !== false));
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

let toastTimer;
function toast(text, action, onAction) {
  document.querySelectorAll(".toast").forEach((e) => e.remove());
  const el = h("div", { class: "toast" }, text, action ? h("button", { onclick: () => { el.remove(); onAction(); } }, action) : null);
  document.body.append(el);
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.remove(), action ? 6000 : 3000);
}

// ---------- the server ----------

const api = {
  token: localStorage.getItem("trunk.token"),
  setToken(tok) {
    this.token = tok;
    if (tok) localStorage.setItem("trunk.token", tok);
    else localStorage.removeItem("trunk.token");
    // For <img> and <video>: trunk takes the token from this cookie for reading only.
    const secure = location.protocol === "https:" ? "; Secure" : "";
    document.cookie = tok ? `trunk_token=${tok}; Path=/; SameSite=Strict${secure}` : "trunk_token=; Path=/; Max-Age=0";
  },
  async req(method, path, body, raw) {
    const headers = { Authorization: "Bearer " + this.token };
    if (body !== undefined && !raw) headers["Content-Type"] = "application/json";
    const res = await fetch("/api/v1" + path, { method, headers, body: raw ? body : body === undefined ? undefined : JSON.stringify(body) });
    if (res.status === 401) {
      this.setToken(null);
      location.reload();
      throw new Error("signed out");
    }
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
    return res.status === 204 ? null : res.json();
  },
  get(p) { return this.req("GET", p); },
  post(p, b) { return this.req("POST", p, b); },
  thumb: (a, size = "medium") => `/api/v1/assets/${a.id}/thumb/${size}?v=${(a.sha256 || "").slice(0, 12)}`,
  file: (a) => `/api/v1/assets/${a.id}/file?v=${(a.sha256 || "").slice(0, 12)}`,
  face: (id) => `/api/v1/faces/${id}/thumb`,
};

// ---------- the library ----------

const lib = {
  assets: new Map(), // id → asset (one's own, trashed ones too)
  albums: new Map(),
  shared: new Map(), // album id → its assets, for albums with other people's photos
  people: [],
  cursor: 0,
  me: null,
  async sync() {
    for (;;) {
      const page = await api.get(`/changes?since=${this.cursor}&limit=1000`);
      for (const a of page.assets) a.deleted ? this.assets.delete(a.id) : this.assets.set(a.id, a);
      for (const al of page.albums) al.deleted ? this.albums.delete(al.id) : this.albums.set(al.id, al);
      this.cursor = page.cursor;
      if (!page.more) break;
    }
    this.people = (await api.get("/people").catch(() => ({ people: [] }))).people || [];
  },
  // Newest first, like the phone.
  live() { return [...this.assets.values()].filter((a) => !a.trashed_at).sort(byTaken); },
  async albumAssets(al) {
    if (!al.owner && !(al.members || []).length) return al.asset_ids.map((id) => this.assets.get(id)).filter((a) => a && !a.trashed_at);
    if (!this.shared.has(al.id)) this.shared.set(al.id, (await api.get(`/albums/${al.id}/assets`)).assets);
    return this.shared.get(al.id).filter((a) => !a.trashed_at);
  },
};
const byTaken = (a, b) => (b.taken_at || "").localeCompare(a.taken_at || "") || b.id - a.id;

// ---------- search: what the phone matches ----------

function searchText(a) {
  if (a._text) return a._text;
  const d = a.local_time ? new Date(a.local_time) : null;
  const month = d ? [d.toLocaleString("de-DE", { month: "long" }), d.toLocaleString("en", { month: "long" }), String(d.getFullYear())] : [];
  const p = a.place || {};
  a._text = [a.name, a.caption, p.city, p.region, p.country, a.make, a.model, ...month, a.kind === "video" ? "video" : "foto photo"]
    .filter(Boolean).join(" ").toLowerCase();
  return a._text;
}
function search(q) {
  const ws = q.toLowerCase().split(/\s+/).filter(Boolean);
  if (!ws.length) return [];
  const extra = new Map(); // asset id → album and people names
  const add = (id, s) => extra.set(id, (extra.get(id) || "") + " " + s.toLowerCase());
  for (const al of lib.albums.values()) for (const id of al.asset_ids || []) add(id, al.name || "");
  for (const p of lib.people) if (p.name) for (const id of p.asset_ids) add(id, p.name);
  return lib.live().filter((a) => ws.every((w) => searchText(a).includes(w) || (extra.get(a.id) || "").includes(w)));
}

// ---------- the grid: rows at the photos' own proportions, per day ----------

const selection = new Set();
let shownList = []; // what the open grid shows, for the viewer and range selection

function dayLabel(d) {
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const day = new Date(d); day.setHours(0, 0, 0, 0);
  const diff = (today - day) / 864e5;
  if (diff === 0) return t("today");
  if (diff === 1) return t("yesterday");
  const opts = { weekday: "short", day: "numeric", month: "long" };
  if (day.getFullYear() !== today.getFullYear()) opts.year = "numeric";
  return day.toLocaleDateString(locale, opts);
}

function ratio(a) { return a.width > 0 && a.height > 0 ? Math.min(Math.max(a.width / a.height, 0.4), 3) : 1; }

function grid(list, root, { headers = true } = {}) {
  shownList = list;
  root.innerHTML = "";
  if (!list.length) return;
  const width = root.clientWidth || window.innerWidth;
  const target = parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--row")) || 220;
  const gap = 4;
  // Days, newest first; without headers one long run.
  const groups = [];
  for (const [i, a] of list.entries()) {
    const key = headers && a.local_time ? a.local_time.slice(0, 10) : "";
    if (!groups.length || groups.at(-1).key !== key) groups.push({ key, items: [] });
    groups.at(-1).items.push([i, a]);
  }
  const frag = document.createDocumentFragment();
  for (const g of groups) {
    const sec = h("section", { class: "day" });
    if (g.key) sec.append(h("h3", {}, dayLabel(g.key)));
    let row = [], sum = 0;
    const flush = (last) => {
      if (!row.length) return;
      // Fill the width, but a short last row keeps the target height.
      let hgt = (width - gap * (row.length - 1)) / sum;
      if (last && hgt > target * 1.25) hgt = target;
      const el = h("div", { class: "row" });
      for (const [i, a] of row) el.append(tile(a, i, Math.floor(ratio(a) * hgt), Math.floor(hgt)));
      sec.append(el);
      row = []; sum = 0;
    };
    for (const it of g.items) {
      row.push(it); sum += ratio(it[1]);
      if ((width - gap * (row.length - 1)) / sum < target) flush(false);
    }
    flush(true);
    frag.append(sec);
  }
  root.append(frag);
}

function tile(a, i, w, hgt) {
  const el = h("div", { class: "tile" + (selection.has(a.id) ? " sel" : ""), style: `--w:${w}px;--h:${hgt}px`, "data-id": a.id });
  const img = h("img", { loading: "lazy", decoding: "async", alt: "", src: api.thumb(a, hgt > 300 ? "preview" : "medium") });
  img.addEventListener("load", () => img.classList.add("in"), { once: true });
  el.append(img, h("span", { class: "check" }));
  if (a.kind === "video") el.append(h("span", { class: "badge", html: svg("play") + esc(duration(a.duration_ms)) }));
  else if (a.motion) el.append(h("span", { class: "badge", html: svg("motion") }));
  if (a.favorite) el.insertAdjacentHTML("beforeend", svg("heart", "fav"));
  el.addEventListener("click", (e) => {
    if (e.target.classList.contains("check") || selection.size || e.shiftKey || e.ctrlKey || e.metaKey) toggleSelect(a, i, e.shiftKey);
    else viewer(shownList, i);
  });
  return el;
}

const duration = (ms) => { const s = Math.round((ms || 0) / 1000); return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`; };

let lastPicked = -1;
function toggleSelect(a, i, range) {
  if (range && lastPicked >= 0) {
    const [lo, hi] = [Math.min(lastPicked, i), Math.max(lastPicked, i)];
    for (let k = lo; k <= hi; k++) selection.add(shownList[k].id);
  } else if (selection.has(a.id)) selection.delete(a.id);
  else selection.add(a.id);
  lastPicked = i;
  document.querySelectorAll(".tile").forEach((el) => el.classList.toggle("sel", selection.has(+el.dataset.id)));
  selbar();
}

// ---------- selection actions ----------

function selbar() {
  document.querySelectorAll(".selbar").forEach((e) => e.remove());
  document.body.classList.toggle("selecting", selection.size > 0);
  if (!selection.size) return;
  const ids = [...selection];
  const items = ids.map((id) => lib.assets.get(id)).filter(Boolean);
  const inTrash = route().name === "trash";
  const allFav = items.length && items.every((a) => a.favorite);
  const btn = (icon, label, fn) => h("button", { class: "icon-btn", title: label, "aria-label": label, html: svg(icon), onclick: fn });
  const bar = h("div", { class: "selbar" }, h("b", {}, `${ids.length} ${t("selected")}`));
  if (inTrash) {
    bar.append(btn("restore", t("restore"), async () => { await api.post("/assets/restore", { ids }); done(); }));
    bar.append(btn("trash", t("deleteForever"), async () => { await api.post("/assets/delete", { ids }); done(); }));
  } else {
    bar.append(btn("heart", allFav ? t("unfavorite") : t("favorite"), async () => {
      for (const id of ids) await api.req("PATCH", `/assets/${id}`, { favorite: !allFav });
      done();
    }));
    bar.append(btn("album", t("addTo"), () => albumPicker(ids)));
    bar.append(btn("download", t("download"), () => { for (const a of items) download(a); }));
    bar.append(btn("trash", t("delete"), async () => {
      await api.post("/assets/trash", { ids });
      done();
      toast(t("trashed"), t("undo"), async () => { await api.post("/assets/restore", { ids }); await refresh(); });
    }));
  }
  bar.append(btn("close", t("cancel"), () => { selection.clear(); render(); }));
  document.body.append(bar);
  async function done() { selection.clear(); await refresh(); }
}

function download(a) {
  const link = h("a", { href: api.file(a) + "&download=1", download: a.name });
  document.body.append(link); link.click(); link.remove();
}

function albumPicker(ids) {
  const back = h("div", { class: "dialog-back", onclick: (e) => e.target === back && back.remove() });
  const list = h("div", { class: "list" });
  const own = [...lib.albums.values()].filter((al) => al.kind !== "direct" && (al.can_add || al.role === "owner"));
  for (const al of own) {
    const cover = lib.assets.get((al.asset_ids || [])[0]);
    list.append(h("button", { onclick: async () => {
      back.remove();
      await api.post(`/albums/${al.id}/add`, { ids, end: true });
      selection.clear(); toast(`${t("added")}: ${al.name}`); await refresh();
    } }, cover ? h("img", { src: api.thumb(cover, "small"), alt: "" }) : h("img", { alt: "" }), h("span", {}, al.name)));
  }
  const name = h("input", { class: "field", placeholder: t("albumName") });
  back.append(h("div", { class: "dialog" }, h("h3", {}, t("addTo")), list,
    h("div", { class: "row-end" }, name, h("button", { class: "btn", onclick: async () => {
      if (!name.value.trim()) return name.focus();
      back.remove();
      await api.post("/albums", { name: name.value.trim(), asset_ids: ids });
      selection.clear(); toast(`${t("added")}: ${name.value.trim()}`); await refresh();
    } }, t("create")))));
  document.body.append(back);
}

// ---------- the viewer ----------

function viewer(list, start) {
  let i = start, info = innerWidth > 900 && localStorage.getItem("trunk.info") === "1";
  const v = h("div", { class: "viewer", tabindex: "-1" });
  const stage = h("div", { class: "stage" });
  const aside = h("aside");
  const bar = h("div", { class: "bar" });
  const prev = h("button", { class: "arrow prev", "aria-label": "previous", html: svg("prev"), onclick: () => go(-1) });
  const next = h("button", { class: "arrow next", "aria-label": "next", html: svg("next"), onclick: () => go(1) });
  stage.append(bar, prev, next);
  v.append(stage, aside);
  document.body.append(v);
  v.focus();
  history.pushState({ viewer: true }, "");
  const close = () => { v.remove(); removeEventListener("popstate", onPop); render(); };
  const onPop = () => close();
  addEventListener("popstate", onPop);
  v.addEventListener("keydown", (e) => {
    if (e.target.tagName === "TEXTAREA" || e.target.tagName === "INPUT") return;
    if (e.key === "ArrowLeft") go(-1);
    else if (e.key === "ArrowRight") go(1);
    else if (e.key === "Escape") history.back();
    else if (e.key === "i") { info = !info; show(); }
  });
  let x0 = null;
  stage.addEventListener("touchstart", (e) => { x0 = e.touches[0].clientX; }, { passive: true });
  stage.addEventListener("touchend", (e) => {
    if (x0 == null) return;
    const dx = e.changedTouches[0].clientX - x0;
    if (Math.abs(dx) > 60) go(dx < 0 ? 1 : -1);
    x0 = null;
  });
  function go(d) { const n = i + d; if (n >= 0 && n < list.length) { i = n; show(); } }
  async function show() {
    const a = list[i];
    stage.querySelectorAll("img.main, video").forEach((e) => e.remove());
    let media;
    if (a.kind === "video") media = h("video", { class: "main", src: api.file(a), controls: true, autoplay: true, playsinline: true });
    else media = h("img", { class: "main", src: api.thumb(a, "preview"), alt: a.caption || a.name });
    stage.prepend(media);
    prev.disabled = i === 0; next.disabled = i === list.length - 1;
    const icon = (name, label, fn, on) => h("button", { class: "icon-btn" + (on ? " on" : ""), title: label, "aria-label": label, html: svg(name), onclick: fn });
    fill(bar,
      icon("close", t("cancel"), () => history.back()),
      h("div", { class: "name" }, a.name),
      a.motion ? icon("motion", t("motion"), () => playMotion(a)) : null,
      a.owner ? null : icon("heart", t("favorite"), async () => {
        const res = await api.req("PATCH", `/assets/${a.id}`, { favorite: !a.favorite });
        Object.assign(a, res); lib.assets.set(a.id, a); show();
      }, a.favorite),
      icon("download", t("download"), () => download(a)),
      icon("info", t("details"), () => { info = !info; localStorage.setItem("trunk.info", info ? "1" : "0"); show(); }),
      a.owner ? null : icon("trash", t("delete"), async () => {
        await api.post("/assets/trash", { ids: [a.id] });
        a.trashed_at = new Date().toISOString();
        list.splice(i, 1);
        if (!list.length) return history.back();
        i = Math.min(i, list.length - 1); show();
        toast(t("trashed"), t("undo"), async () => { await api.post("/assets/restore", { ids: [a.id] }); await refresh(); });
      }),
    );
    aside.classList.toggle("hidden", !info);
    if (info) details(a);
  }
  function playMotion(a) {
    const vid = h("video", { src: `/api/v1/assets/${a.id}/motion`, autoplay: true, playsinline: true, muted: true, style: "position:absolute;max-width:100%;max-height:100vh" });
    vid.addEventListener("ended", () => vid.remove());
    vid.addEventListener("click", () => vid.remove());
    stage.append(vid);
  }
  async function details(a) {
    const d = a.local_time ? new Date(a.local_time) : null;
    const p = a.place || {};
    const cam = [a.make, a.model].filter(Boolean).join(" ");
    const facts = [a.f_number && `ƒ/${a.f_number}`, a.exposure && (a.exposure < 1 ? `1/${Math.round(1 / a.exposure)} s` : `${a.exposure} s`),
      a.iso && `ISO ${a.iso}`, a.focal_length && `${Math.round(a.focal_length)} mm`].filter(Boolean).join(" · ");
    const caption = h("textarea", { placeholder: t("captionHint"), disabled: !!a.owner }, a.caption || "");
    caption.addEventListener("change", async () => {
      const res = await api.req("PATCH", `/assets/${a.id}`, { caption: caption.value });
      Object.assign(a, res); a._text = null;
    });
    fill(aside,
      h("h4", {}, t("caption")), caption,
      d ? h("h4", {}, t("taken")) : null,
      d ? h("p", {}, d.toLocaleString(locale, { dateStyle: "full", timeStyle: "short" })) : null,
      p.city ? h("h4", {}, t("place")) : null,
      p.city ? h("p", {}, [p.city, p.region, p.country].filter(Boolean).join(", ")) : null,
      cam ? h("h4", {}, t("camera")) : null,
      cam ? h("p", {}, cam) : null, facts ? h("p", { style: "color:var(--muted)" }, facts) : null,
      h("h4", {}, t("file")),
      h("p", {}, `${a.name}`), h("p", { style: "color:var(--muted)" }, `${a.width} × ${a.height} · ${(a.size / 1048576).toFixed(1)} MB`),
      a.owner ? h("p", { style: "color:var(--muted)" }, `${t("sharedBy")} ${a.owner}`) : null,
    );
    if (a.kind === "image" && !a.owner) {
      const res = await api.get(`/assets/${a.id}/faces`).catch(() => null);
      const named = (res?.faces || []).filter((f) => f.person);
      if (named.length && list[i] === a) {
        const faces = h("div", { class: "faces" });
        for (const f of named) faces.append(h("a", { href: `#/person/${f.person}`, onclick: () => v.remove() },
          h("img", { src: api.face(f.id), alt: "" }), f.name || t("nameIt")));
        aside.append(h("h4", {}, t("inPhoto")), faces);
      }
    }
  }
  show();
}

// ---------- upload ----------

async function upload(files) {
  const list = [...files].filter((f) => /^(image|video)\//.test(f.type) || /\.(heic|heif|dng|avif)$/i.test(f.name));
  if (!list.length) return;
  let n = 0, failed = [];
  for (const f of list) {
    toast(`${t("uploading")} ${n + 1}/${list.length}: ${f.name}`);
    try {
      await api.req("POST", `/assets?filename=${encodeURIComponent(f.name)}&mtime=${f.lastModified}`, f, true);
      n++;
    } catch (e) {
      failed.push(`${f.name}: ${e.message.includes("too large") ? t("tooBig") : e.message}`);
    }
  }
  toast(`${n} ${t("uploaded")}` + (failed.length ? ` · ${failed.join(", ")}` : ""));
  await refresh();
}

let dragDepth = 0;
addEventListener("dragenter", (e) => {
  if (!api.token || ![...e.dataTransfer.types].includes("Files")) return;
  if (dragDepth++ === 0) document.body.append(h("div", { class: "drop" }, t("dropHere")));
});
addEventListener("dragleave", () => { if (--dragDepth <= 0) { dragDepth = 0; $(".drop")?.remove(); } });
addEventListener("dragover", (e) => e.preventDefault());
addEventListener("drop", (e) => { e.preventDefault(); dragDepth = 0; $(".drop")?.remove(); if (api.token) upload(e.dataTransfer.files); });

// ---------- views ----------

function route() {
  const [, name = "", arg = ""] = location.hash.split("/");
  return { name: name || "photos", arg: decodeURIComponent(arg) };
}

let query = "";

function frame() {
  const app = $("#app");
  if ($("header.top", app)) return $("main", app);
  const input = h("input", { type: "search", placeholder: t("search"), value: query, "aria-label": t("search") });
  input.addEventListener("input", () => { query = input.value; if (route().name !== "photos") location.hash = "#/"; else render(); });
  const picker = h("input", { type: "file", multiple: true, accept: "image/*,video/*", class: "hidden", onchange: () => upload(picker.files) });
  app.replaceChildren(
    h("header", { class: "top" },
      h("a", { class: "brand", href: "#/", html: mark + "<span>trunk</span>" }),
      h("nav", { class: "main" },
        h("a", { href: "#/", "data-r": "photos" }, t("photos")),
        h("a", { href: "#/albums", "data-r": "albums" }, t("albums")),
        h("a", { href: "#/people", "data-r": "people" }, t("people")),
        h("a", { href: "#/favorites", "data-r": "favorites" }, t("favorites")),
        h("a", { href: "#/trash", "data-r": "trash" }, t("trash"))),
      h("label", { class: "search", html: svg("search") }, input),
      picker,
      h("button", { class: "icon-btn", title: t("upload"), "aria-label": t("upload"), html: svg("upload"), onclick: () => picker.click() }),
      h("button", { class: "icon-btn", title: t("logout"), "aria-label": t("logout"), html: svg("logout"), onclick: async () => {
        await api.req("POST", "/auth/logout").catch(() => {});
        api.setToken(null); location.hash = ""; location.reload();
      } })),
    h("main"));
  return $("main", app);
}

function render() {
  if (document.querySelector(".viewer")) return;
  const main = frame();
  const r = route();
  document.querySelectorAll("nav.main a").forEach((a) => a.classList.toggle("on", a.dataset.r === r.name));
  const gridRoot = () => h("div", { class: "grid" });
  main.replaceChildren();
  const title = (text, sub) => { main.append(h("h2", { class: "title" }, text)); if (sub) main.append(h("p", { class: "sub" }, sub)); };
  const showGrid = (list, empty, opts) => {
    if (!list.length) return main.append(h("p", { class: "empty" }, empty));
    const g = gridRoot(); main.append(g); grid(list, g, opts);
  };
  switch (r.name) {
    case "photos": {
      if (query.trim()) {
        const found = search(query);
        title(`„${query.trim()}“`, count(found.length));
        showGrid(found, t("found"));
      } else showGrid(lib.live(), t("nothing"));
      break;
    }
    case "favorites": title(t("favorites")); showGrid(lib.live().filter((a) => a.favorite), t("nothing")); break;
    case "trash": {
      const list = [...lib.assets.values()].filter((a) => a.trashed_at).sort((a, b) => b.trashed_at.localeCompare(a.trashed_at));
      title(t("trash"), t("emptyTrash"));
      showGrid(list, t("nothing"), { headers: false });
      break;
    }
    case "albums": {
      title(t("albums"));
      const cards = h("div", { class: "cards" });
      const albums = [...lib.albums.values()].sort((a, b) => (b.updated_at || "").localeCompare(a.updated_at || ""));
      for (const al of albums) {
        const cover = lib.assets.get((al.asset_ids || []).find((id) => lib.assets.get(id) && !lib.assets.get(id).trashed_at));
        const name = al.kind === "direct" ? (al.role === "owner" ? `→ ${al.name}` : `${t("sharedBy")} ${al.owner?.name || ""}`) : al.name;
        cards.append(h("a", { class: "card", href: `#/album/${al.id}` },
          h("div", { class: "cover" }, cover ? h("img", { src: api.thumb(cover, "medium"), loading: "lazy", alt: "" }) : null),
          h("b", {}, name), h("span", {}, count((al.asset_ids || []).length))));
      }
      if (!albums.length) main.append(h("p", { class: "empty" }, t("nothing")));
      main.append(cards);
      break;
    }
    case "album": {
      const al = lib.albums.get(+r.arg);
      if (!al) { location.hash = "#/albums"; return; }
      title(al.name || "", al.owner ? `${t("sharedBy")} ${al.owner.name}` : null);
      const g = gridRoot(); main.append(g);
      lib.albumAssets(al).then((list) => { if (route().arg === r.arg) list.length ? grid(list.sort(byTaken), g) : g.append(h("p", { class: "empty" }, t("nothing"))); });
      break;
    }
    case "people": {
      title(t("people"));
      const cards = h("div", { class: "cards people" });
      for (const p of lib.people.filter((p) => !p.hidden)) {
        cards.append(h("a", { class: "card", href: `#/person/${p.id}` },
          h("div", { class: "cover" }, h("img", { src: api.face(p.cover), loading: "lazy", alt: "" })),
          p.name ? h("b", {}, p.name) : h("b", { class: "unnamed" }, t("nameIt")), h("span", {}, count(p.asset_ids.length))));
      }
      if (!lib.people.length) main.append(h("p", { class: "empty" }, t("nothing")));
      main.append(cards);
      break;
    }
    case "person": {
      const p = lib.people.find((p) => p.id === +r.arg);
      if (!p) { location.hash = "#/people"; return; }
      const name = h("input", { value: p.name, placeholder: t("typeName"), "aria-label": t("typeName") });
      name.addEventListener("change", async () => {
        await api.req("PATCH", `/people/${p.id}`, { name: name.value });
        p.name = name.value.trim();
        toast(p.name || t("unnamed"));
      });
      main.append(h("div", { class: "person-head" }, h("img", { src: api.face(p.cover), alt: "" }), h("div", {}, name, h("p", { class: "sub", style: "margin:4px 0 0" }, count(p.asset_ids.length)))));
      showGrid(p.asset_ids.map((id) => lib.assets.get(id)).filter((a) => a && !a.trashed_at), t("nothing"));
      break;
    }
    default:
      location.hash = "#/";
  }
  selbar();
}

async function refresh() {
  lib.shared.clear();
  await lib.sync();
  render();
}

let resizeTimer;
addEventListener("resize", () => { clearTimeout(resizeTimer); resizeTimer = setTimeout(() => { if (!document.querySelector(".viewer")) render(); }, 200); });
addEventListener("hashchange", () => { selection.clear(); render(); scrollTo(0, 0); });
addEventListener("keydown", (e) => {
  if (e.key === "Escape" && selection.size && !document.querySelector(".viewer")) { selection.clear(); render(); }
});
// Others' changes, and the phone's: look again when the tab comes back.
document.addEventListener("visibilitychange", () => { if (!document.hidden && api.token) refresh().catch(() => {}); });

// ---------- start ----------

function loginView() {
  const user = h("input", { class: "field", autocomplete: "username", required: true });
  const pass = h("input", { class: "field", type: "password", autocomplete: "current-password", required: true });
  const err = h("div", { class: "error" });
  const form = h("form", {}, h("div", { html: mark, style: "display:grid" }), h("h1", {}, "trunk"),
    h("label", {}, t("user"), user), h("label", {}, t("password"), pass), err, h("button", { class: "btn" }, t("login")));
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    err.textContent = "";
    const ua = navigator.userAgent.match(/(Firefox|Chrome|Safari|Edg)\/[\d.]+/)?.[1] || "Browser";
    const res = await fetch("/api/v1/auth/login", { method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ user: user.value, password: pass.value, device: `${ua} (web)` }) });
    if (!res.ok) { err.textContent = t("wrong"); return; }
    api.setToken((await res.json()).token);
    start();
  });
  $("#app").replaceChildren(h("div", { class: "login" }, form));
  user.focus();
}

async function start() {
  if (!api.token) return loginView();
  api.setToken(api.token); // the cookie, in case the browser dropped it
  $("#app").replaceChildren(h("p", { class: "empty" }, t("loading")));
  try {
    lib.me = await api.get("/me");
    await lib.sync();
  } catch (e) {
    if (!api.token) return loginView();
    $("#app").replaceChildren(h("p", { class: "empty" }, e.message));
    return;
  }
  render();
}

document.documentElement.lang = de ? "de" : "en";
start();
