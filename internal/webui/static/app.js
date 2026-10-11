"use strict";
// FileSync web UI: render trang quản lý từ JSON API của daemon.
// Thuần JS, không framework, không request ra ngoài. Mọi thay đổi
// trạng thái là POST; thao tác phá huỷ luôn confirm() trước — và
// server tự kiểm tra lại mọi điều kiện (không tin client).

const $ = (id) => document.getElementById(id);

function esc(s) {
  // Thoát đủ 5 ký tự: dữ liệu từ xa (tên peer, đường dẫn file) cũng
  // được nội suy vào thuộc tính nháy đơn (data-name='...' v.v.) nên
  // thiếu ' là vỡ thuộc tính — lỗ hổng XSS round 1 job 006.
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
}

async function api(path, opts) {
  const res = await fetch(path, opts);
  let body = null;
  try { body = await res.json(); } catch (e) { /* lỗi tĩnh */ }
  if (!res.ok) throw new Error((body && body.error) || ("HTTP " + res.status));
  return body;
}
function post(path, data) {
  return api(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data || {}),
  });
}

/* ---------- thông báo + trạng thái chờ ---------- */
function toast(msg, kind) {
  const box = $("toasts");
  const el = document.createElement("div");
  el.className = "toast " + (kind === "err" ? "toast-err" : "toast-ok");
  el.textContent = msg;
  box.appendChild(el);
  setTimeout(() => el.remove(), 5200);
}

// withBusy: khoá nút + hiện trạng thái chờ trong lúc gọi API.
async function withBusy(btn, fn) {
  if (!btn) return fn();
  const old = btn.innerHTML;
  btn.disabled = true;
  btn.innerHTML = "<span class='spin'>◌</span> Đang xử lý…";
  try { return await fn(); }
  finally { btn.disabled = false; btn.innerHTML = old; }
}

/* ---------- định dạng ---------- */
function fmtBytes(n) {
  if (n < 1024) return n + " B";
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n;
  for (const u of units) { v /= 1024; if (v < 1024) return v.toFixed(1) + " " + u; }
  return v.toFixed(1) + " PiB";
}
function fmtTime(unixNano) {
  if (!unixNano) return "chưa từng";
  return new Date(unixNano / 1e6).toLocaleString("vi-VN");
}
// Fingerprint: tách thành từng nhóm 4 ký tự để mắt đối chiếu dễ.
function fingerprintHTML(fp) {
  return "<div class='fingerprint'>" +
    esc(fp).split(" ").map((g) => "<span>" + g + "</span>").join("") + "</div>";
}

/* ---------- Tổng quan ---------- */
async function loadStatus() {
  let st;
  try { st = await api("/api/status"); }
  catch (e) {
    setPill("pill-red", "Mất kết nối tới daemon");
    return;
  }
  window._lastStatus = st;
  if (st.paused) setPill("pill-amber", "Đã tạm dừng đồng bộ");
  else if (st.state === "running") setPill("pill-green", "Đang đồng bộ");
  else setPill("pill-gray", st.state);
  $("header-device").textContent = st.device_name + " · " + st.device_id;
  $("stat-state").textContent = st.paused ? "Tạm dừng" : (st.state === "running" ? "Đang chạy" : st.state);
  $("stat-files").textContent = st.indexed_files;
  $("stat-sent").textContent = fmtBytes(st.bytes_sent);
  $("stat-recv").textContent = fmtBytes(st.bytes_received);
  $("stat-rate").textContent = fmtBytes(st.rateBps || st.rate_bps || 0) + "/s";
  $("btn-pause").disabled = !!st.paused;
  $("btn-resume").disabled = !st.paused;
  $("overview-body").innerHTML =
    "<dl class='kv'>" +
    kv("Tên thiết bị", esc(st.device_name)) +
    kv("Device ID", "<span class='mono'>" + esc(st.device_id) + "</span>") +
    kv("Địa chỉ lắng nghe", "<span class='mono'>" + esc(st.listen_addr) + "</span>") +
    kv("Thư mục đồng bộ", esc(st.sync_dir)) +
    kv("Số vòng đã đồng bộ", st.rounds) +
    "</dl><div class='hint'>Fingerprint của máy này (đọc cho máy kia đối chiếu khi ghép đôi):</div>" +
    fingerprintHTML(st.fingerprint);
  const warn = $("skip-warning");
  if (st.skipped_version_archives > 0) {
    const last = st.last_version_skip;
    warn.textContent = "⚠ " + st.skipped_version_archives + " lần ghi đè file lớn đã KHÔNG lưu bản cũ vào lịch sử phiên bản (file vượt hạn mức lưu trữ)" +
      (last ? ": gần nhất là " + last.path + " (" + fmtBytes(last.size) + ")." : ".");
    warn.classList.remove("hidden");
  } else warn.classList.add("hidden");
  const qb = $("queue-box");
  if (st.queue && st.queue.length) {
    qb.innerHTML = "<div class='card'><h2>Đang truyền</h2>" + st.queue.map((q) => {
      const pct = q.BytesTotal > 0 ? Math.round(100 * q.BytesDone / q.BytesTotal) : 0;
      return "<div class='mono'>" + esc(q.Path) + " — " + pct + "% (" + esc(q.State) + ")</div>";
    }).join("") + "</div>";
  } else qb.innerHTML = "";
  const errs = $("recent-errors");
  if (st.recent_errors && st.recent_errors.length) {
    errs.innerHTML = "<h2>Lỗi gần đây</h2>" + st.recent_errors.map((e) =>
      "<div class='error-text mono'>[" + esc(e.Code) + "] " + esc(e.Path) + " — " + esc(e.Message) + "</div>").join("");
  } else errs.innerHTML = "";
}
function kv(k, v) { return "<dt>" + k + "</dt><dd>" + v + "</dd>"; }
function setPill(cls, text) {
  const p = $("conn-state");
  p.className = "pill " + cls;
  p.innerHTML = "<span class='dot'></span>" + esc(text);
}

/* ---------- Máy đã ghép đôi ---------- */
async function loadPeers() {
  const peers = await api("/api/peers");
  if (!peers.length) {
    $("peers-body").innerHTML = "<div class='card empty'>Chưa ghép đôi với máy nào.<br>Hãy sang thẻ “Ghép đôi” để bắt đầu.</div>";
    return;
  }
  $("peers-body").innerHTML = peers.map((p) => {
    const dot = p.revoked ? "dot-red" : (p.connected ? "dot-green" : "dot-gray");
    const state = p.revoked ? "Đã thu hồi quyền" : (p.connected ? "Đang kết nối" : "Ngoại tuyến");
    return "<div class='card peer-card'><div class='peer-main'>" +
      "<div class='peer-name'><span class='status-dot " + dot + "'></span>" + esc(p.name) +
      " <span class='hint'>(" + state + ")</span></div>" +
      "<div class='mono'>Device ID: " + esc(p.device_id) + "</div>" +
      fingerprintHTML(p.fingerprint || "") +
      "<div class='hint'>Địa chỉ: <span class='mono'>" + esc((p.addresses || []).join(", ") || "—") +
      "</span> · Vòng đồng bộ gần nhất: " + fmtTime(p.last_round_at) + "</div></div>" +
      (p.revoked ? "" : "<button class='btn btn-danger' data-revoke='" + esc(p.device_id) + "' data-name='" + esc(p.name) + "'>Thu hồi</button>") +
      "</div>";
  }).join("");
  document.querySelectorAll("[data-revoke]").forEach((btn) => {
    btn.addEventListener("click", () => withBusy(btn, async () => {
      const id = btn.getAttribute("data-revoke");
      if (!confirm("Thu hồi quyền của máy “" + btn.getAttribute("data-name") + "”?\nMáy đó sẽ bị ngắt kết nối ngay và không đồng bộ được nữa. Muốn dùng lại phải ghép đôi lại từ đầu.")) return;
      try { await post("/api/peers/revoke", { device_id: id }); toast("Đã thu hồi quyền của máy."); loadPeers(); }
      catch (e) { toast(e.message, "err"); }
    }));
  });
}

/* ---------- Ghép đôi ---------- */
let codeExpiresAt = 0;
async function loadPending() {
  const pending = await api("/api/pending");
  const badge = $("pending-count");
  badge.textContent = pending.length || "";
  badge.classList.toggle("hidden", !pending.length);
  if (!pending.length) { $("pending-body").textContent = "Không có yêu cầu nào."; return; }
  $("pending-body").innerHTML = pending.map((p) =>
    "<div class='card pending-card'><div class='peer-name'>⏳ " + esc(p.name) +
    " <span class='hint'>đang xin ghép đôi</span></div>" +
    "<div class='mono'>Device ID: " + esc(p.device_id) + "</div>" +
    "<div class='hint'>Fingerprint của máy kia — đối chiếu kỹ từng nhóm với màn hình của máy đó:</div>" +
    fingerprintHTML(p.fingerprint) +
    "<div class='actions'>" +
    "<button class='btn btn-good btn-big' data-confirm='" + esc(p.session_id) + "'>✓ Duyệt — fingerprint khớp</button>" +
    "<button class='btn btn-danger btn-big' data-reject='" + esc(p.session_id) + "'>✕ Từ chối</button>" +
    "</div></div>"
  ).join("");
  document.querySelectorAll("[data-confirm]").forEach((btn) => {
    btn.addEventListener("click", () => withBusy(btn, async () => {
      try {
        await post("/api/pair/confirm", { session_id: btn.getAttribute("data-confirm") });
        toast("Đã duyệt. Ghép đôi sẽ hoàn tất khi máy kia cũng duyệt.");
        loadPending(); loadPeers();
      } catch (e) { toast(e.message, "err"); }
    }));
  });
  document.querySelectorAll("[data-reject]").forEach((btn) => {
    btn.addEventListener("click", () => withBusy(btn, async () => {
      if (!confirm("Từ chối yêu cầu ghép đôi này?")) return;
      try { await post("/api/pair/reject", { session_id: btn.getAttribute("data-reject") }); toast("Đã từ chối yêu cầu."); loadPending(); }
      catch (e) { toast(e.message, "err"); }
    }));
  });
}

/* ---------- Xung đột ---------- */
async function loadConflicts() {
  const list = await api("/api/conflicts");
  const badge = $("conflict-count");
  badge.textContent = list.length || "";
  badge.classList.toggle("hidden", !list.length);
  if (!list.length) {
    $("conflicts-body").innerHTML = "<div class='card empty'>Không có xung đột nào. 🎉</div>";
    return;
  }
  $("conflicts-body").innerHTML =
    "<p class='hint'>Xung đột xảy ra khi hai máy cùng sửa một file lúc mất kết nối. Không bản nào bị mất — hãy chọn bản giữ lại ở tên gốc; bản còn lại sẽ vào thùng rác và lịch sử phiên bản.</p>" +
    list.map((c, i) =>
      "<div class='card conflict-card'><div class='peer-name'>⚠ <span class='mono'>" + esc(c.original) + "</span></div>" +
      "<table><tr><th>Bản ở tên gốc</th><th>Bản xung đột</th></tr>" +
      "<tr><td>" + fmtBytes(c.size_original) + "</td><td class='mono'>" + esc(c.conflict) + "<br>" +
      fmtBytes(c.size_conflict) + " · từ máy " + esc(c.loser_device) + "</td></tr></table>" +
      "<div class='actions'>" +
      "<button class='btn btn-primary' data-keep='mine' data-i='" + i + "'>Giữ bản ở tên gốc</button>" +
      "<button class='btn' data-keep='theirs' data-i='" + i + "'>Giữ bản xung đột</button>" +
      "</div></div>"
    ).join("");
  document.querySelectorAll("[data-keep]").forEach((btn) => {
    btn.addEventListener("click", () => withBusy(btn, async () => {
      const c = list[Number(btn.getAttribute("data-i"))];
      const keep = btn.getAttribute("data-keep");
      if (!confirm("Giải quyết xung đột của file " + c.original + ": giữ " +
        (keep === "mine" ? "bản đang ở tên gốc" : "bản xung đột") + "?\nBản còn lại vẫn được giữ trong thùng rác/lịch sử.")) return;
      try {
        await post("/api/conflicts/resolve", { original: c.original, conflict: c.conflict, keep: keep });
        toast("Đã giải quyết xung đột của " + c.original + ".");
        loadConflicts();
      } catch (e) { toast(e.message, "err"); }
    }));
  });
}

/* ---------- Lịch sử phiên bản ---------- */
async function loadFiles() {
  const files = await api("/api/files");
  $("files-list").innerHTML = files.map((f) => "<option value='" + esc(f.path) + "'>").join("");
}
async function loadHistory() {
  const path = $("history-path").value.trim();
  if (!path) { toast("Hãy nhập đường dẫn file trước.", "err"); return; }
  let versions;
  try { versions = await api("/api/history?path=" + encodeURIComponent(path)); }
  catch (e) { toast(e.message, "err"); return; }
  if (!versions.length) {
    $("history-body").innerHTML = "<p class='hint'>File này chưa có phiên bản lưu trữ nào.</p>";
    return;
  }
  $("history-body").innerHTML = "<table><tr><th>Phiên bản</th><th>Kích thước</th><th>Thời gian lưu</th><th></th></tr>" +
    versions.map((v) =>
      "<tr><td class='mono'>" + esc(v.name) + "</td><td>" + fmtBytes(v.size) + "</td><td>" + esc(v.mod_time) + "</td>" +
      "<td><button class='btn' data-restore='" + esc(v.name) + "'>Khôi phục</button></td></tr>"
    ).join("") + "</table>";
  document.querySelectorAll("[data-restore]").forEach((btn) => {
    btn.addEventListener("click", () => withBusy(btn, async () => {
      const version = btn.getAttribute("data-restore");
      if (!confirm("Khôi phục file " + path + " về phiên bản " + version + "?\nNội dung hiện tại sẽ được lưu vào lịch sử trước nên khôi phục này hoàn tác được.")) return;
      try { await post("/api/restore", { path: path, version: version }); toast("Đã khôi phục phiên bản."); loadHistory(); }
      catch (e) { toast(e.message, "err"); }
    }));
  });
}

/* ---------- khởi tạo + sự kiện ---------- */
document.querySelectorAll("nav .tab").forEach((btn) => {
  btn.addEventListener("click", () => {
    document.querySelectorAll("nav .tab").forEach((b) => b.classList.remove("active"));
    document.querySelectorAll(".panel").forEach((p) => p.classList.remove("active"));
    btn.classList.add("active");
    $("tab-" + btn.getAttribute("data-tab")).classList.add("active");
    const tab = btn.getAttribute("data-tab");
    if (tab === "peers") loadPeers().catch((e) => toast(e.message, "err"));
    if (tab === "conflicts") loadConflicts().catch((e) => toast(e.message, "err"));
    if (tab === "history") loadFiles().catch(() => {});
    if (tab === "pairing") loadPending().catch(() => {});
  });
});
$("btn-pause").addEventListener("click", (ev) => withBusy(ev.currentTarget, async () => {
  try { await post("/api/pause"); toast("Đã tạm dừng đồng bộ."); loadStatus(); }
  catch (e) { toast(e.message, "err"); }
}));
$("btn-resume").addEventListener("click", (ev) => withBusy(ev.currentTarget, async () => {
  try { await post("/api/resume"); toast("Đã tiếp tục đồng bộ."); loadStatus(); }
  catch (e) { toast(e.message, "err"); }
}));
$("btn-code").addEventListener("click", (ev) => withBusy(ev.currentTarget, async () => {
  try {
    const r = await post("/api/pair/code");
    codeExpiresAt = r.expires_at;
    $("code-value").textContent = r.code.split("").join(" ");
    $("code-display").classList.remove("hidden");
    tickCodeExpiry();
    toast("Đã tạo mã ghép đôi. Đọc mã cho máy kia nhập vào.");
  } catch (e) { toast(e.message, "err"); }
}));
function tickCodeExpiry() {
  if (!codeExpiresAt) return;
  const left = codeExpiresAt - Math.floor(Date.now() / 1000);
  $("code-expiry").textContent = left > 0
    ? "Mã còn hiệu lực " + Math.floor(left / 60) + ":" + String(left % 60).padStart(2, "0")
    : "Mã đã hết hạn — hãy tạo mã mới.";
}
setInterval(tickCodeExpiry, 1000);
$("btn-dial").addEventListener("click", (ev) => withBusy(ev.currentTarget, async () => {
  const addr = $("dial-addr").value.trim();
  const code = $("dial-code").value.replace(/\s+/g, "");
  if (!addr || !code) { toast("Hãy nhập địa chỉ máy kia và mã 6 số.", "err"); return; }
  try {
    await post("/api/pair/dial", { addr: addr, code: code });
    $("dial-msg").textContent = "";
    toast("Đã kết nối tới máy kia. Hãy đối chiếu fingerprint trong mục chờ duyệt bên dưới rồi bấm Duyệt.");
    loadPending();
  } catch (e) { toast(e.message, "err"); }
}));
$("btn-history").addEventListener("click", (ev) => withBusy(ev.currentTarget, loadHistory));

loadStatus().catch(() => {});
loadConflicts().catch(() => {});
loadPending().catch(() => {});
setInterval(() => {
  loadStatus().catch(() => {});
  loadPending().catch(() => {});
}, 3000);
