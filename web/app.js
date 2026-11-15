/* VidGrab — frontend vanilla tanpa framework. */
"use strict";

const $ = (sel) => document.querySelector(sel);

const QUALITY_LABEL = { audio: "MP3" };

let current = null;        // hasil analisis metadata
let selectedFormat = "720";
let pollTimer = null;      // polling riwayat saat ada job aktif
let me = null;             // {authenticated, email, plan, used_today, remaining, ...}
let authMode = "login";    // login | register

/* ---------- util ---------- */

function fmtBytes(n) {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 10 || i === 0 ? 0 : 1) + " " + units[i];
}

function fmtIDR(n) {
  if (!n) return "Gratis";
  return "Rp " + n.toLocaleString("id-ID");
}

function fmtDuration(s) {
  if (!s && s !== 0) return "";
  const m = Math.floor(s / 60), sec = Math.floor(s % 60);
  return m + ":" + String(sec).padStart(2, "0");
}

function statusLabel(status) {
  const map = {
    queued: "Menunggu",
    running: "Mengunduh",
    completed: "Selesai",
    failed: "Gagal",
    canceled: "Dibatalkan",
    expired: "Kedaluwarsa",
  };
  return map[status] || status;
}

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
  let data = null;
  try { data = await res.json(); } catch { /* body bukan JSON */ }
  if (!res.ok) {
    const msg = (data && data.error && data.error.message) || "Terjadi kesalahan";
    throw Object.assign(new Error(msg), { code: data && data.error && data.error.code });
  }
  return data;
}

/* ---------- sesi & kuota (header) ---------- */

async function refreshMe() {
  try { me = await api("/api/me"); } catch { me = null; }
  renderHeader();
  renderQuota();
  renderPlans(); // status tombol bergantung plan aktif
}

function renderHeader() {
  const el = $("#header-actions");
  el.innerHTML = "";
  if (me && me.authenticated) {
    const badge = document.createElement("span");
    badge.className = "plan-badge";
    badge.textContent = me.plan.name;
    const email = document.createElement("span");
    email.className = "user-email";
    email.textContent = me.email;
    const out = document.createElement("button");
    out.className = "btn ghost small";
    out.textContent = "Keluar";
    out.addEventListener("click", async () => {
      try { await api("/api/auth/logout", { method: "POST" }); } catch { /* */ }
      hideAuth();
      await refreshMe();
      await refreshHistory();
    });
    el.append(badge, email, out);
  } else {
    const btn = document.createElement("button");
    btn.className = "btn small";
    btn.textContent = "Masuk";
    btn.addEventListener("click", () => showAuth("login"));
    el.appendChild(btn);
  }
}

function renderQuota() {
  const el = $("#quota");
  if (!me) { el.textContent = ""; return; }
  el.textContent =
    `${me.plan.name} — ${me.remaining}/${me.plan.daily_limit} unduhan hari ini`;
}

/* ---------- auth ---------- */

function showAuth(mode) {
  authMode = mode;
  $("#auth-title").textContent = mode === "login" ? "Masuk" : "Daftar";
  $("#auth-submit").textContent = mode === "login" ? "Masuk" : "Daftar";
  $("#auth-switch-label").textContent = mode === "login" ? "Belum punya akun?" : "Sudah punya akun?";
  $("#auth-toggle").textContent = mode === "login" ? "Daftar" : "Masuk";
  $("#auth-error").classList.add("hidden");
  $("#auth").classList.remove("hidden");
  $("#auth").scrollIntoView({ behavior: "smooth", block: "center" });
}

function hideAuth() {
  $("#auth").classList.add("hidden");
}

$("#auth-toggle").addEventListener("click", (e) => {
  e.preventDefault();
  showAuth(authMode === "login" ? "register" : "login");
});

$("#auth-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const btn = $("#auth-submit");
  btn.disabled = true;
  const path = authMode === "login" ? "/api/auth/login" : "/api/auth/register";
  try {
    await api(path, {
      method: "POST",
      body: JSON.stringify({
        email: $("#auth-email").value.trim(),
        password: $("#auth-pass").value,
      }),
    });
    hideAuth();
    $("#auth-form").reset();
    await refreshMe();
    await refreshHistory(); // riwayat anonim di-claim ke akun
  } catch (err) {
    const el = $("#auth-error");
    el.textContent = err.message;
    el.classList.remove("hidden");
  } finally {
    btn.disabled = false;
  }
});

/* ---------- analisis URL ---------- */

$("#analyze-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const url = $("#url").value.trim();
  if (!url) return;

  const btn = $("#analyze-btn");
  btn.disabled = true;
  btn.textContent = "Menganalisis…";
  $("#analyze-error").classList.add("hidden");
  $("#result").classList.add("hidden");

  try {
    current = await api("/api/metadata", {
      method: "POST",
      body: JSON.stringify({ url }),
    });
    renderResult();
  } catch (err) {
    const el = $("#analyze-error");
    el.textContent = err.message;
    el.classList.remove("hidden");
  } finally {
    btn.disabled = false;
    btn.textContent = "Analisis";
  }
});

function renderResult() {
  const card = $("#result");
  card.classList.remove("hidden");
  $("#result-thumb").src = current.thumbnail || "";
  $("#result-title").textContent = current.title;
  $("#result-meta").textContent = [
    current.provider,
    fmtDuration(current.duration),
  ].filter(Boolean).join(" • ");

  const chips = $("#result-chips");
  chips.innerHTML = "";
  selectedFormat = "";

  const heights = (current.heights || [])
    .filter((h) => h > 0 && h <= 2160)
    .sort((a, b) => b - a)
    .slice(0, 4);
  const options = [...heights, "audio"];

  for (const opt of options) {
    const chip = document.createElement("button");
    chip.type = "button";
    chip.className = "chip";
    const isAudio = opt === "audio";
    const allowed = isAudio || opt <= current.max_height;
    chip.textContent = (QUALITY_LABEL[opt] || opt + "p") + (allowed ? "" : " 🔒");
    chip.disabled = !allowed;
    chip.dataset.format = String(opt);
    chip.addEventListener("click", () => {
      selectedFormat = String(opt);
      chips.querySelectorAll(".chip").forEach((c) => c.classList.remove("active"));
      chip.classList.add("active");
    });
    chips.appendChild(chip);
    if (!selectedFormat && allowed) {
      selectedFormat = String(opt);
      chip.classList.add("active");
    }
  }
  $("#result-error").classList.add("hidden");
}

$("#download-btn").addEventListener("click", async () => {
  if (!current || !selectedFormat) return;
  const btn = $("#download-btn");
  btn.disabled = true;
  btn.textContent = "Memulai…";
  $("#result-error").classList.add("hidden");

  try {
    await api("/api/downloads", {
      method: "POST",
      body: JSON.stringify({ url: current.url, format: selectedFormat }),
    });
    $("#result").classList.add("hidden");
    $("#url").value = "";
    current = null;
    await refreshHistory();
    refreshMe();
  } catch (err) {
    const el = $("#result-error");
    if (err.code === "quota_exceeded" || err.code === "quality_not_allowed") {
      el.textContent = err.message + " — lihat paket di bawah untuk upgrade.";
    } else {
      el.textContent = err.message;
    }
    el.classList.remove("hidden");
  } finally {
    btn.disabled = false;
    btn.textContent = "Unduh sekarang";
  }
});

/* ---------- riwayat & polling ---------- */

async function refreshHistory() {
  let items = [];
  try {
    const data = await api("/api/downloads");
    items = data.items || [];
  } catch { /* abaikan error polling */ }
  renderHistory(items);

  const active = items.some((i) => i.status === "queued" || i.status === "running");
  if (active && !pollTimer) {
    pollTimer = setInterval(refreshHistory, 2000);
  } else if (!active && pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
}

function renderHistory(items) {
  const list = $("#history-list");
  list.innerHTML = "";

  if (!items.length) {
    const empty = document.createElement("div");
    empty.className = "empty";
    empty.textContent = "Belum ada unduhan. Tempel link di atas untuk memulai.";
    list.appendChild(empty);
    return;
  }

  for (const it of items) {
    const row = document.createElement("div");
    row.className = "download-item card";

    const thumb = document.createElement("div");
    thumb.className = "dl-thumb";
    if (it.thumbnail) {
      const img = document.createElement("img");
      img.src = it.thumbnail;
      img.alt = "";
      img.loading = "lazy";
      thumb.appendChild(img);
    }
    row.appendChild(thumb);

    const info = document.createElement("div");
    info.className = "dl-info";

    const title = document.createElement("div");
    title.className = "dl-title";
    title.textContent = it.title || shortURL(it.url);
    title.title = it.url;

    const sub = document.createElement("div");
    sub.className = "dl-sub";
    const parts = [
      it.provider,
      it.format === "audio" ? "MP3" : it.format + "p",
    ];
    if (it.status === "completed") parts.push(fmtBytes(it.file_size));
    if (it.status === "failed" && it.error_message) {
      parts.push(it.error_message);
    }
    sub.textContent = parts.filter(Boolean).join(" • ");
    info.append(title, sub);

    if (it.status === "queued" || it.status === "running") {
      const wrap = document.createElement("div");
      wrap.style.marginTop = "8px";
      wrap.style.display = "flex";
      wrap.style.alignItems = "center";
      wrap.style.gap = "10px";

      const prog = document.createElement("div");
      prog.className = "progress";
      const bar = document.createElement("div");
      bar.className = "progress-bar";
      bar.style.width = (it.status === "queued" ? 0 : it.progress) + "%";
      prog.appendChild(bar);

      const pct = document.createElement("span");
      pct.className = "pct";
      pct.textContent = it.status === "queued" ? "Menunggu…" : it.progress + "%";

      wrap.append(prog, pct);
      info.appendChild(wrap);
    }

    if (it.status === "completed" && it.expires_at) {
      const exp = document.createElement("div");
      exp.className = "dl-sub";
      exp.textContent = "File dihapus otomatis: " + new Date(it.expires_at).toLocaleString("id-ID");
      exp.style.marginTop = "6px";
      info.appendChild(exp);
    }
    row.appendChild(info);

    const actions = document.createElement("div");
    actions.className = "dl-actions";

    const badge = document.createElement("span");
    badge.className = "badge " + (it.status === "running" ? "running" : it.status);
    badge.textContent = statusLabel(it.status);
    actions.appendChild(badge);

    if (it.status === "completed") {
      const a = document.createElement("a");
      a.className = "btn primary small";
      a.href = it.file_url || "/api/files/" + it.id;
      a.textContent = "Unduh file";
      if (it.filename) a.setAttribute("download", it.filename);
      actions.appendChild(a);
    } else if (it.status === "queued" || it.status === "running") {
      const cancel = document.createElement("button");
      cancel.className = "btn ghost small";
      cancel.textContent = "Batalkan";
      cancel.addEventListener("click", async () => {
        try { await api("/api/downloads/" + it.id, { method: "DELETE" }); } catch { /* */ }
        refreshHistory();
      });
      actions.appendChild(cancel);
    } else if (it.status === "failed") {
      const retry = document.createElement("button");
      retry.className = "btn ghost small";
      retry.textContent = "Coba lagi";
      retry.addEventListener("click", async () => {
        try {
          await api("/api/downloads", {
            method: "POST",
            body: JSON.stringify({ url: it.url, format: it.format }),
          });
          refreshHistory();
          refreshMe();
        } catch (err) { alert(err.message); }
      });
      actions.appendChild(retry);
    }
    row.appendChild(actions);
    list.appendChild(row);
  }
}

function shortURL(u) {
  if (!u) return "";
  return u.length > 60 ? u.slice(0, 57) + "…" : u;
}

/* ---------- paket & langganan ---------- */

async function refreshPlans() {
  let plans = [];
  try {
    const data = await api("/api/plans");
    plans = data.items || [];
  } catch { /* */ }
  renderPlans(plans);
}

function renderPlans(plans) {
  const list = $("#plan-list");
  if (!plans) return; // belum ada data
  list.innerHTML = "";

  for (const p of plans) {
    const card = document.createElement("div");
    card.className = "plan-card card";
    if (me && me.plan && me.plan.code === p.code) {
      card.classList.add("current");
    }

    const name = document.createElement("div");
    name.className = "plan-name";
    name.textContent = p.name;

    const price = document.createElement("div");
    price.className = "plan-price";
    price.textContent = fmtIDR(p.price_idr) + (p.price_idr ? " / bulan" : "");

    const feats = document.createElement("div");
    feats.className = "plan-feats";
    const featLines = [
      `${p.daily_limit} unduhan / hari`,
      `hingga ${p.max_height}p` + (p.max_height >= 1080 ? " (4K menyusul)" : ""),
      `file tersimpan ${p.retention_hours} jam`,
    ];
    if (p.price_idr) featLines.push("tanpa iklan (segera)");
    feats.innerHTML = "";
    for (const line of featLines) {
      const d = document.createElement("div");
      d.textContent = "✓ " + line;
      feats.appendChild(d);
    }

    const isCurrent = me && me.plan && me.plan.code === p.code;
    const action = document.createElement("div");
    action.className = "plan-action";

    if (isCurrent) {
      const tag = document.createElement("div");
      tag.className = "current-tag";
      tag.textContent = "✓ Paket aktif";
      action.appendChild(tag);
      if (p.price_idr > 0) {
        const cancel = document.createElement("button");
        cancel.className = "btn ghost small";
        cancel.textContent = "Batalkan langganan";
        cancel.addEventListener("click", async () => {
          if (!confirm("Batalkan langganan? Manfaat Pro langsung berhenti.")) return;
          try {
            await api("/api/billing/cancel", { method: "POST" });
            await refreshMe();
          } catch (err) { alert(err.message); }
        });
        action.appendChild(cancel);
      }
    } else if (p.price_idr > 0) {
      const btn = document.createElement("button");
      btn.className = "btn primary wide";
      btn.textContent = "Berlangganan";
      btn.addEventListener("click", async () => {
        if (!me || !me.authenticated) {
          showAuth("login");
          return;
        }
        btn.disabled = true;
        try {
          const res = await api("/api/billing/checkout", {
            method: "POST",
            body: JSON.stringify({ plan_code: p.code }),
          });
          if (res.payment_url) {
            // Mode gateway: buka Snap di tab baru, lalu polling status.
            window.open(res.payment_url, "_blank");
            await pollPayment(res.payment_id);
            await refreshMe();
          } else {
            alert("Langganan " + p.name + " aktif! Selamat menikmati. 🎉");
            await refreshMe();
          }
        } catch (err) {
          alert(err.message);
        } finally {
          btn.disabled = false;
        }
      });
      action.appendChild(btn);
    }

    card.append(name, price, feats, action);
    list.appendChild(card);
  }
}

/* ---------- init ---------- */

// pollPayment: polling status payment setiap 3 detik (maks ~3 menit)
// sampai paid / failed, atau user menutup halaman Snap.
function pollPayment(id) {
  return new Promise((resolve) => {
    let elapsed = 0;
    const iv = setInterval(async () => {
      elapsed += 3;
      try {
        const pm = await api("/api/billing/payments/" + id);
        if (pm.status === "paid") {
          clearInterval(iv);
          alert("Pembayaran diterima — langganan aktif! 🎉");
          resolve();
        } else if (pm.status === "failed" || pm.status === "expired") {
          clearInterval(iv);
          alert("Pembayaran gagal atau dibatalkan. Coba lagi ya.");
          resolve();
        }
      } catch { /* biarkan polling lanjut */ }
      if (elapsed >= 180) {
        clearInterval(iv);
        alert("Menunggu pembayaran terlalu lama. Jika sudah membayar, " +
              "halaman ini bisa diperbarui setelah notifikasi masuk.");
        resolve();
      }
    }, 3000);
  });
}

refreshMe();
refreshHistory();
refreshPlans();
