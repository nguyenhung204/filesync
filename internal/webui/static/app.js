"use strict";
// FileSync web UI: renders the management page from the daemon's
// JSON API. No framework, no external requests. State changes are
// POST-only and every destructive action asks confirm() first —
// the server re-checks everything regardless.

const $ = (id) => document.getElementById(id);

function esc(s) {
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

async function api(path, opts) {
  const res = await fetch(path, opts);
  let body = null;
  try { body = await res.json(); } catch (e) { /* static errors */ }
  if (!res.ok) {
    throw new Error((body && body.error) || ("HTTP " + res.status));
  }
  return body;
}

function post(path, data) {
  return api(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data || {}),
  });
}

let toastTimer = null;
function toast(msg, isError) {
  const t = $("toast");
  t.textContent = msg;
  t.className = isError ? "error" : "";
  t.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), 5000);
}

function fmtBytes(n) {
  if (n < 1024) return n + " B";
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n;
  for (const u of units) { v /= 1024; if (v < 1024) return v.toFixed(1) + " " + u; }
  return v.toFixed(1) + " PiB";
}

function fmtTime(unixNano) {
  if (!unixNano) return "chưa từng";
  return new Date(unixNano / 1e6).toLocaleString();
}

// ---------- Tổng quan ----------
async function loadStatus() {
  let st;
  try {
    st = await api("/api/status");
  } catch (e) {
    $("conn-state").textContent = "mất kết nối tới daemon";
    return;
  }
  const stateTxt = st.paused ? "đã tạm dừng" : (st.state === "running" ? "đang chạy" : st.state);
  $("conn-state").textContent = stateTxt;
  $("btn-pause").disabled = !!st.paused;
  $("btn-resume").disabled = !st.paused;
  $("overview-body").innerHTML =
    "<table>" +
    row("Thiết bị", esc(st.device_name) + " <span class='mono'>" + esc(st.device_id) + "</span>") +
    row("Fingerprint", "<span class='mono'>" + esc(st.fingerprint) + "</span>") +
    row("Địa chỉ lắng nghe", esc(st.listen_addr)) +
    row("Thư mục đồng bộ", esc(st.sync_dir)) +
    row("Số file đã lập chỉ mục", st.indexed_files) +
    row("Đã truyền", fmtBytes(st.bytes_sent) + " gửi, " + fmtBytes(st.bytes_received) +
      " nhận, " + st.rounds + " vòng, tốc độ hiện tại " + fmtBytes(st.rate_bps) + "/s") +
    "</table>";
  const warn = $("skip-warning");
  if (st.skipped_version_archives > 0) {
    const last = st.last_version_skip;
    warn.textContent = "Cảnh báo: " + st.skipped_version_archives +
      " lần ghi đè file lớn đã KHÔNG lưu bản cũ vào lịch sử phiên bản (file vượt hạn mức lưu trữ)" +
      (last ? (": gần nhất là " + last.path + " (" + fmtBytes(last.size) + ").") : ".");
    warn.classList.remove("hidden");
  } else {
    warn.classList.add("hidden");
  }
  const errs = $("recent-errors");
  if (st.recent_errors && st.recent_errors.length) {
    errs.innerHTML = "<h2>Lỗi gần đây</h2>" + st.recent_errors.map((e) =>
      "<div class='error mono'>[" + esc(e.Code) + "] " + esc(e.Path) + " — " + esc(e.Message) + "</div>").join("");
  } else {
    errs.innerHTML = "";
  }
}
function row(k, v) { return "<tr><th>" + k + "</th><td>" + v + "</td></tr>"; }

// ---------- Máy đã pair ----------
async function loadPeers() {
  const peers = await api("/api/peers");
  if (!peers.length) { $("peers-body").textContent = "Chưa ghép với máy nào."; return; }
  $("peers-body").innerHTML = "<table><tr><th>Máy</th><th>Device ID</th><th>Fingerprint</th>" +
    "<th>Địa chỉ</th><th>Trạng thái</th><th>Vòng gần nhất</th><th></th></tr>" +
    peers.map((p) =>
      "<tr><td>" + esc(p.name) + "</td><td class='mono'>" + esc(p.device_id) + "</td>" +
      "<td class='mono'>" + esc(p.fingerprint) + "</td><td class='mono'>" + esc((p.addresses || []).join(", ")) + "</td>" +
      "<td>" + (p.revoked ? "đã thu hồi" : (p.connected ? "đang kết nối" : "ngoại tuyến")) + "</td>" +
      "<td>" + fmtTime(p.last_round_at) + "</td>" +
      "<td>" + (p.revoked ? "" : "<button data-revoke='" + esc(p.device_id) + "'>Thu hồi</button>") + "</td></tr>"
    ).join("") + "</table>";
  document.querySelectorAll("[data-revoke]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const id = btn.getAttribute("data-revoke");
      if (!confirm("Thu hồi máy " + id + "?\nMáy đó sẽ bị ngắt kết nối và không đồng bộ được nữa (ghép đôi lại nếu muốn dùng tiếp).")) return;
      try { await post("/api/peers/revoke", { device_id: id }); toast("Đã thu hồi máy."); loadPeers(); }
      catch (e) { toast(e.message, true); }
    });
  });
}

// ---------- Ghép đôi ----------
async function loadPending() {
  const pending = await api("/api/pending");
  if (!pending.length) { $("pending-body").textContent = "Không có yêu cầu nào."; return; }
  $("pending-body").innerHTML = pending.map((p) =>
    "<div class='warn'><div><strong>" + esc(p.name) + "</strong> <span class='mono'>" + esc(p.device_id) + "</span></div>" +
    "<div>Fingerprint của máy kia: <span class='mono'>" + esc(p.fingerprint) + "</span></div>" +
    "<div class='row'><button data-confirm='" + esc(p.session_id) + "'>Duyệt — fingerprint khớp</button>" +
    "<button data-reject='" + esc(p.session_id) + "'>Từ chối</button></div></div>"
  ).join("");
  document.querySelectorAll("[data-confirm]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      try { await post("/api/pair/confirm", { session_id: btn.getAttribute("data-confirm") }); toast("Đã duyệt. Ghép đôi hoàn tất khi phía kia cũng duyệt."); loadPending(); loadPeers(); }
      catch (e) { toast(e.message, true); }
    });
  });
  document.querySelectorAll("[data-reject]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      if (!confirm("Từ chối yêu cầu ghép đôi này?")) return;
      try { await post("/api/pair/reject", { session_id: btn.getAttribute("data-reject") }); toast("Đã từ chối."); loadPending(); }
      catch (e) { toast(e.message, true); }
    });
  });
}

// ---------- Conflicts ----------
async function loadConflicts() {
  const list = await api("/api/conflicts");
  $("conflict-count").textContent = list.length ? "(" + list.length + ")" : "";
  if (!list.length) { $("conflicts-body").textContent = "Không có conflict nào."; return; }
  $("conflicts-body").innerHTML = "<table><tr><th>File gốc</th><th>Bản conflict</th><th></th></tr>" +
    list.map((c, i) =>
      "<tr><td class='mono'>" + esc(c.original) + " (" + fmtBytes(c.size_original) + ")</td>" +
      "<td class='mono'>" + esc(c.conflict) + " (" + fmtBytes(c.size_conflict) + ", từ máy " + esc(c.loser_device) + ")</td>" +
      "<td><button data-keep='mine' data-i='" + i + "'>Giữ bản của tôi</button> " +
      "<button data-keep='theirs' data-i='" + i + "'>Giữ bản conflict</button></td></tr>"
    ).join("") + "</table>" +
    "<p class='hint'>Bản không được giữ sẽ vào thùng rác + lịch sử phiên bản — không mất dữ liệu.</p>";
  document.querySelectorAll("[data-keep]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const c = list[Number(btn.getAttribute("data-i"))];
      const keep = btn.getAttribute("data-keep");
      if (!confirm("Giải quyết conflict của " + c.original + ": giữ " +
        (keep === "mine" ? "bản của máy này" : "bản conflict") + "?")) return;
      try {
        await post("/api/conflicts/resolve", { original: c.original, conflict: c.conflict, keep: keep });
        toast("Đã giải quyết conflict."); loadConflicts();
      } catch (e) { toast(e.message, true); }
    });
  });
}

// ---------- Lịch sử ----------
async function loadFiles() {
  const files = await api("/api/files");
  $("files-list").innerHTML = files.map((f) => "<option value='" + esc(f.path) + "'>").join("");
}

async function loadHistory() {
  const path = $("history-path").value.trim();
  if (!path) { toast("Nhập đường dẫn file trước.", true); return; }
  let versions;
  try { versions = await api("/api/history?path=" + encodeURIComponent(path)); }
  catch (e) { toast(e.message, true); return; }
  if (!versions.length) { $("history-body").textContent = "File này chưa có phiên bản lưu trữ nào."; return; }
  $("history-body").innerHTML = "<table><tr><th>Phiên bản</th><th>Kích thước</th><th>Thời gian</th><th></th></tr>" +
    versions.map((v) =>
      "<tr><td class='mono'>" + esc(v.name) + "</td><td>" + fmtBytes(v.size) + "</td><td>" + esc(v.mod_time) + "</td>" +
      "<td><button data-restore='" + esc(v.name) + "'>Khôi phục</button></td></tr>"
    ).join("") + "</table>";
  document.querySelectorAll("[data-restore]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const version = btn.getAttribute("data-restore");
      if (!confirm("Khôi phục " + path + " về phiên bản " + version + "?\nNội dung hiện tại sẽ được lưu vào lịch sử trước nên khôi phục này hoàn tác được.")) return;
      try {
        await post("/api/restore", { path: path, version: version });
        toast("Đã khôi phục."); loadHistory();
      } catch (e) { toast(e.message, true); }
    });
  });
}

// ---------- Khởi tạo ----------
document.querySelectorAll("nav .tab").forEach((btn) => {
  btn.addEventListener("click", () => {
    document.querySelectorAll("nav .tab").forEach((b) => b.classList.remove("active"));
    document.querySelectorAll(".panel").forEach((p) => p.classList.remove("active"));
    btn.classList.add("active");
    $("tab-" + btn.getAttribute("data-tab")).classList.add("active");
    const tab = btn.getAttribute("data-tab");
    if (tab === "peers") loadPeers().catch((e) => toast(e.message, true));
    if (tab === "conflicts") loadConflicts().catch((e) => toast(e.message, true));
    if (tab === "history") loadFiles().catch(() => {});
    if (tab === "pairing") loadPending().catch(() => {});
  });
});

$("btn-pause").addEventListener("click", async () => {
  try { await post("/api/pause"); toast("Đã tạm dừng đồng bộ."); loadStatus(); } catch (e) { toast(e.message, true); }
});
$("btn-resume").addEventListener("click", async () => {
  try { await post("/api/resume"); toast("Đã tiếp tục đồng bộ."); loadStatus(); } catch (e) { toast(e.message, true); }
});
$("btn-code").addEventListener("click", async () => {
  try {
    const r = await post("/api/pair/code");
    $("code-display").innerHTML = "Mã: <span class='bigcode'>" + esc(r.code) + "</span> (hết hạn lúc " +
      new Date(r.expires_at * 1000).toLocaleTimeString() + ")";
  } catch (e) { toast(e.message, true); }
});
$("btn-dial").addEventListener("click", async () => {
  const addr = $("dial-addr").value.trim();
  const code = $("dial-code").value.replace(/\s+/g, "");
  if (!addr || !code) { toast("Nhập địa chỉ và mã 6 số.", true); return; }
  try {
    await post("/api/pair/dial", { addr: addr, code: code });
    $("dial-msg").textContent = "Đã kết nối. Đối chiếu fingerprint của máy kia trong mục 'Yêu cầu đang chờ duyệt' bên dưới rồi duyệt.";
    loadPending();
  } catch (e) { $("dial-msg").textContent = e.message; $("dial-msg").className = "error"; }
});
$("btn-history").addEventListener("click", loadHistory);

loadStatus().catch(() => {});
loadConflicts().catch(() => {});
setInterval(() => { loadStatus().catch(() => {}); loadPending().catch(() => {}); }, 3000);
