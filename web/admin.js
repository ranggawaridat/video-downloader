/* VidGrab Admin — vanilla JS, gate login + 4 tab. */
"use strict";

const $ = (sel) => document.querySelector(sel);

let me = null;
let plans = [];

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
  let data = null;
  try { data = await res.json(); } catch { /* */ }
  if (!res.ok) {
    const msg = (data && data.error && data.error.message) || "Terjadi kesalahan";
    throw Object.assign(new Error(msg), { code: data && data.error && data.error.code, status: res.status });
  }
  return data;
}

function fmtBytes(n) {
  if (!n || n <= 0) return "0";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 10 || i === 0 ? 0 : 1) + " " + units[i];
}
function fmtIDR(n) { return "Rp " + (n || 0).toLocaleString("id-ID"); }
function fmtDate(s) { return s ? new Date(s).toLocaleString("id-ID") : ""; }

/* ---------- gate ---------- */

async function checkAccess() {
  try { me = await api("/api/me"); } catch { me = null; }
  const isAdmin = me && me.authenticated && me.role === "admin";
  $("#admin-gate").classList.toggle("hidden", isAdmin);
  $("#admin-app").classList.toggle("hidden", !isAdmin);
  renderHeaderUser(isAdmin);
  if (isAdmin) {
    await loadPlansForEditor();
    loadOverview();
  }
  return isAdmin;
}

function renderHeaderUser(isAdmin) {
  const el = $("#admin-user");
  el.innerHTML = "";
  if (isAdmin) {
    const email = document.createElement("span");
    email.className = "user-email";
    email.textContent = me.email;
    const out = document.createElement("button");
    out.className = "btn ghost small";
    out.textContent = "Keluar";
    out.addEventListener("click", async () => {
      try { await api("/api/auth/logout", { method: "POST" }); } catch { /* */ }
      checkAccess();
    });
    el.append(email, out);
  }
}

$("#admin-login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("#gate-error").classList.add("hidden");
  try {
    await api("/api/auth/login", {
      method: "POST",
      body: JSON.stringify({
        email: $("#gate-email").value.trim(),
        password: $("#gate-pass").value,
      }),
    });
    await checkAccess();
  } catch (err) {
    const el = $("#gate-error");
    el.textContent = err.message;
    el.classList.remove("hidden");
  }
});

/* ---------- tabs ---------- */

document.querySelectorAll(".tab").forEach((btn) => {
  btn.addEventListener("click", () => {
    document.querySelectorAll(".tab").forEach((b) => b.classList.remove("active"));
    btn.classList.add("active");
    for (const name of ["overview", "users", "downloads", "plans"]) {
      $("#tab-" + name).classList.toggle("hidden", name !== btn.dataset.tab);
    }
    if (btn.dataset.tab === "overview") loadOverview();
    if (btn.dataset.tab === "users") loadUsers();
    if (btn.dataset.tab === "downloads") loadDownloads();
    if (btn.dataset.tab === "plans") loadPlansForEditor();
  });
});

/* ---------- overview ---------- */

async function loadOverview() {
  let data;
  try { data = await api("/api/admin/stats/overview"); } catch (err) { alert(err.message); return; }

  const t = data.totals || {};
  const successRate = t.downloads_24h > 0
    ? Math.round((t.completed_24h / t.downloads_24h) * 100) + "%"
    : "—";
  const stats = [
    ["Total pengguna", t.total_users],
    ["Baru 24 jam", t.new_users_24h],
    ["Unduhan 24 jam", t.downloads_24h],
    ["Sukses 24 jam", successRate],
    ["Antrean (q+run)", t.queue_depth],
    ["Gagal total", t.failed_total],
    ["Langganan aktif", t.active_subs],
    ["MRR", fmtIDR(t.mrr_idr)],
    ["Storage", fmtBytes(t.storage_bytes)],
  ];

  const grid = $("#stat-cards");
  grid.innerHTML = "";
  for (const [label, value] of stats) {
    const card = document.createElement("div");
    card.className = "stat-card";
    const l = document.createElement("div");
    l.className = "stat-label";
    l.textContent = label;
    const v = document.createElement("div");
    v.className = "stat-value";
    v.textContent = value;
    card.append(l, v);
    grid.appendChild(card);
  }

  // chart harian
  const daily = data.daily || [];
  const chart = $("#daily-chart");
  const xAxis = $("#daily-chart");
  chart.innerHTML = "";
  const max = Math.max(1, ...daily.map((d) => d.count));
  const xLabels = document.createElement("div");
  xLabels.className = "daily-x";
  for (const d of daily) {
    const bar = document.createElement("div");
    bar.className = "daily-bar";
    bar.style.height = Math.max(4, (d.count / max) * 120) + "px";
    const tip = document.createElement("span");
    tip.className = "daily-tip";
    tip.textContent = d.day + " — " + d.count;
    bar.appendChild(tip);
    chart.appendChild(bar);

    const x = document.createElement("span");
    x.textContent = d.day.slice(8); // hanya tanggal
    xLabels.appendChild(x);
  }
  chart.after(xLabels);

  // provider teratas
  const prov = $("#top-providers");
  prov.innerHTML = "";
  const plist = data.top_providers || [];
  if (!plist.length) {
    const e = document.createElement("div");
    e.className = "empty-admin";
    e.textContent = "Belum ada data.";
    prov.appendChild(e);
  }
  for (const p of plist) {
    const row = document.createElement("div");
    row.className = "prov-row";
    const n = document.createElement("span");
    n.textContent = p.provider;
    const c = document.createElement("span");
    c.className = "prov-count";
    c.textContent = p.count;
    row.append(n, c);
    prov.appendChild(row);
  }

  // failures + retry
  const ft = $("#failures-table");
  ft.innerHTML = "";
  const failures = data.recent_failures || [];
  if (!failures.length) {
    const e = document.createElement("div");
    e.className = "empty-admin";
    e.textContent = "Tidak ada kegagalan. 🎉";
    ft.appendChild(e);
    return;
  }
  ft.appendChild(buildFailuresTable(failures));
}

function buildFailuresTable(failures) {
  const tbl = document.createElement("table");
  tbl.className = "tbl";
  tbl.innerHTML = "<thead><tr><th>Judul</th><th>Provider</th><th>Error</th><th class='num'>Attempt</th><th>Waktu</th><th></th></tr></thead>";
  const body = document.createElement("tbody");
  for (const f of failures) {
    const tr = document.createElement("tr");
    const td1 = document.createElement("td");
    td1.textContent = f.title || "—";
    const td2 = document.createElement("td");
    td2.className = "muted";
    td2.textContent = f.provider || "—";
    const td3 = document.createElement("td");
    td3.className = "muted";
    td3.textContent = f.error_code + (f.error_message ? ": " + f.error_message : "");
    td3.title = f.error_message || "";
    const td4 = document.createElement("td");
    td4.className = "muted num";
    td4.textContent = f.attempts;
    const td5 = document.createElement("td");
    td5.className = "muted";
    td5.textContent = fmtDate(f.created_at);
    const td6 = document.createElement("td");
    const retry = document.createElement("button");
    retry.className = "btn small";
    retry.textContent = "Retry";
    retry.addEventListener("click", async () => {
      retry.disabled = true;
      try {
        await api("/api/admin/downloads/" + f.id + "/retry", { method: "POST" });
        loadOverview();
      } catch (err) { alert(err.message); retry.disabled = false; }
    });
    td6.appendChild(retry);
    tr.append(td1, td2, td3, td4, td5, td6);
    body.appendChild(tr);
  }
  tbl.appendChild(body);
  return tbl;
}

/* ---------- users ---------- */

async function loadUsers() {
  const q = $("#user-search").value.trim();
  let data;
  try { data = await api("/api/admin/users?q=" + encodeURIComponent(q)); } catch (err) { alert(err.message); return; }
  const container = $("#users-table");
  container.innerHTML = "";
  const items = data.items || [];
  if (!items.length) {
    const e = document.createElement("div");
    e.className = "empty-admin";
    e.textContent = "Tidak ada pengguna.";
    container.appendChild(e);
    return;
  }

  const tbl = document.createElement("table");
  tbl.className = "tbl";
  tbl.innerHTML = "<thead><tr><th>Email</th><th>Role</th><th>Plan</th><th class='num'>Unduhan</th><th>Terdaftar</th><th>Berikan plan</th></tr></thead>";
  const body = document.createElement("tbody");
  for (const u of items) {
    const tr = document.createElement("tr");

    const tdE = document.createElement("td");
    tdE.textContent = u.email;
    const tdR = document.createElement("td");
    tdR.className = "muted";
    tdR.textContent = u.role;
    const tdP = document.createElement("td");
    const pb = document.createElement("span");
    pb.className = "badge" + (u.plan === "pro" ? " completed" : "");
    pb.textContent = u.plan;
    tdP.appendChild(pb);
    const tdD = document.createElement("td");
    tdD.className = "num";
    tdD.textContent = u.downloads;
    const tdC = document.createElement("td");
    tdC.className = "muted";
    tdC.textContent = fmtDate(u.created_at);

    const tdA = document.createElement("td");
    const sel = document.createElement("select");
    sel.className = "plan-pick";
    const paid = plans.filter((p) => p.price_idr > 0);
    for (const p of paid) {
      const o = document.createElement("option");
      o.value = p.code;
      o.textContent = p.name + " (30 hari)";
      sel.appendChild(o);
    }
    const grant = document.createElement("button");
    grant.className = "btn small";
    grant.textContent = "Aktifkan";
    grant.addEventListener("click", async () => {
      grant.disabled = true;
      try {
        await api("/api/admin/users/" + u.id + "/grant", {
          method: "POST",
          body: JSON.stringify({ plan_code: sel.value, days: 30 }),
        });
        loadUsers();
      } catch (err) { alert(err.message); grant.disabled = false; }
    });
    tdA.append(sel, grant);

    tr.append(tdE, tdR, tdP, tdD, tdC, tdA);
    body.appendChild(tr);
  }
  tbl.appendChild(body);
  container.appendChild(tbl);
}

let searchTimer = null;
$("#user-search").addEventListener("input", () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(loadUsers, 300);
});

/* ---------- downloads ---------- */

async function loadDownloads() {
  const status = $("#dl-status-filter").value;
  let data;
  try { data = await api("/api/admin/downloads?status=" + encodeURIComponent(status)); } catch (err) { alert(err.message); return; }
  const container = $("#downloads-table");
  container.innerHTML = "";
  const items = data.items || [];
  if (!items.length) {
    const e = document.createElement("div");
    e.className = "empty-admin";
    e.textContent = "Tidak ada unduhan.";
    container.appendChild(e);
    return;
  }

  const tbl = document.createElement("table");
  tbl.className = "tbl";
  tbl.innerHTML = "<thead><tr><th>Judul</th><th>Pengguna</th><th>Provider</th><th>Status</th><th class='num'>Attempt</th><th>Waktu</th><th></th></tr></thead>";
  const body = document.createElement("tbody");
  for (const d of items) {
    const tr = document.createElement("tr");
    const td1 = document.createElement("td");
    td1.textContent = d.title || d.id.slice(0, 8);
    td1.title = d.id;
    const td2 = document.createElement("td");
    td2.className = "muted";
    td2.textContent = d.user_email || "anonim";
    const td3 = document.createElement("td");
    td3.className = "muted";
    td3.textContent = d.provider || "—";
    const td4 = document.createElement("td");
    const b = document.createElement("span");
    b.className = "badge" + (d.status === "running" ? " running" : d.status === "completed" ? " completed" : d.status === "failed" ? " failed" : "");
    b.textContent = d.status;
    td4.appendChild(b);
    const td5 = document.createElement("td");
    td5.className = "num muted";
    td5.textContent = d.attempts;
    const td6 = document.createElement("td");
    td6.className = "muted";
    td6.textContent = fmtDate(d.created_at);
    const td7 = document.createElement("td");
    if (d.status === "failed" || d.status === "canceled") {
      const retry = document.createElement("button");
      retry.className = "btn small";
      retry.textContent = "Retry";
      retry.addEventListener("click", async () => {
        retry.disabled = true;
        try {
          await api("/api/admin/downloads/" + d.id + "/retry", { method: "POST" });
          loadDownloads();
        } catch (err) { alert(err.message); retry.disabled = false; }
      });
      td7.appendChild(retry);
    }
    tr.append(td1, td2, td3, td4, td5, td6, td7);
    body.appendChild(tr);
  }
  tbl.appendChild(body);
  container.appendChild(tbl);
}

$("#dl-status-filter").addEventListener("change", loadDownloads);

/* ---------- plans editor ---------- */

async function loadPlansForEditor() {
  try {
    const data = await api("/api/plans");
    plans = data.items || [];
  } catch { plans = []; }
  renderPlansEditor();
}

function renderPlansEditor() {
  const el = $("#plans-editor");
  el.innerHTML = "";
  const wrap = document.createElement("div");
  wrap.className = "plan-edit";

  for (const p of plans) {
    const card = document.createElement("div");
    card.className = "card";
    const title = document.createElement("h3");
    title.textContent = p.code + " — " + p.name;
    card.appendChild(title);

    const fields = [
      ["daily_limit", "Unduhan / hari", p.daily_limit],
      ["max_height", "Resolusi maks", p.max_height],
      ["retention_hours", "Retensi (jam)", p.retention_hours],
      ["price_idr", "Harga (Rp)", p.price_idr],
    ];
    const inputs = {};
    for (const [key, label, value] of fields) {
      const f = document.createElement("div");
      f.className = "field";
      const l = document.createElement("label");
      l.textContent = label;
      const inp = document.createElement("input");
      inp.type = "number";
      inp.value = value;
      inp.min = key === "price_idr" ? 0 : 1;
      inputs[key] = inp;
      f.append(l, inp);
      card.appendChild(f);
    }

    const save = document.createElement("button");
    save.className = "btn primary small";
    save.textContent = "Simpan";
    save.addEventListener("click", async () => {
      save.disabled = true;
      try {
        await api("/api/admin/plans/" + p.code, {
          method: "PATCH",
          body: JSON.stringify({
            daily_limit: Number(inputs.daily_limit.value),
            max_height: Number(inputs.max_height.value),
            retention_hours: Number(inputs.retention_hours.value),
            price_idr: Number(inputs.price_idr.value),
          }),
        });
        await loadPlansForEditor();
      } catch (err) { alert(err.message); }
      save.disabled = false;
    });
    card.appendChild(save);
    wrap.appendChild(card);
  }
  el.appendChild(wrap);
}

/* ---------- init ---------- */

checkAccess();
