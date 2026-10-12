"use strict";
// FileSync web UI — lõi (job 009): helpers, bản đồ lỗi tiếng
// Việt (D7), poll trạng thái 1s, màn hình chính (panel Đang
// chuyển hai chiều, danh sách file, thẻ thiết bị/điều khiển),
// các tab Máy đã ghép đôi / Xung đột / Lịch sử. Ghép nối nằm ở
// pairing.js, kéo-thả ở upload.js. Thuần JS, không framework,
// không request ra ngoài; mọi thay đổi trạng thái là POST.

const $ = (id) => document.getElementById(id);

function esc(s) {
  // Thoát đủ 5 ký tự: dữ liệu từ xa (tên peer, đường dẫn file)
  // cũng được nội suy vào thuộc tính nháy đơn (data-name='...'
  // v.v.) nên thiếu ' là vỡ thuộc tính — lỗ hổng XSS job 006.
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
}

async function api(path, opts) {
  const res = await fetch(path, opts);
  let body = null;
  try { body = await res.json(); } catch (e) { /* lỗi tĩnh */ }
  if (!res.ok) {
    const err = new Error((body && body.error) || ("HTTP " + res.status));
    err.code = body && body.code;
    err.status = res.status;
    throw err;
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

/* ---------- trạng thái dùng chung ---------- */
const FS = {
  status: null,        // StatusView gần nhất
  peers: [],           // PeerView gần nhất
  files: [],           // FileView gần nhất
  onStatus: [],        // hook fn(status) sau mỗi poll status
  onPeers: [],         // hook fn(peers) sau mỗi poll peers
  activeTab: "home",
  peerName(id) {
    const p = FS.peers.find((x) => x.device_id === id);
    if (p && p.name) return p.name;
    return id ? id.slice(0, 8) + "…" : "máy kia";
  },
};

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
function baseName(p) {
  const i = String(p).lastIndexOf("/");
  return i >= 0 ? p.slice(i + 1) : p;
}
// Fingerprint: tách thành từng nhóm 4 ký tự để mắt đối chiếu dễ.
function fingerprintHTML(fp) {
  return "<div class='fingerprint'>" +
    esc(fp).split(" ").map((g) => "<span>" + g + "</span>").join("") + "</div>";
}
function kv(k, v) { return "<dt>" + k + "</dt><dd>" + v + "</dd>"; }

/* ---------- bản đồ lỗi tiếng Việt (D7) ---------- */
// explainError(code, rawMessage) -> {title, hint}. Mọi lỗi hiển
// thị cho người dùng đều qua đây: tiêu đề + gợi ý bằng lời
// thường; thông tin gốc (mã + message) thu vào <details>.
const ERROR_TEXT = {
  disk_full: ["Máy đã hết dung lượng trống",
    "Giải phóng bớt dung lượng trên máy đó rồi FileSync sẽ tự chuyển tiếp, không cần làm gì thêm."],
  chunk_checksum: ["Dữ liệu nhận về không khớp",
    "FileSync sẽ tự tải lại phần bị lỗi ở vòng đồng bộ sau."],
  bad_remote_path: ["Tên file không hợp lệ",
    "Đổi tên file ở máy kia (tránh ký tự đặc biệt cuối tên/ký tự cấm của Windows) rồi chờ đồng bộ lại."],
  locked: ["File đang bị chương trình khác mở",
    "Đóng chương trình đang mở file đích (Word/Excel…) rồi bấm Thử lại."],
  invalid_path: ["Tên hoặc đường dẫn file không hợp lệ",
    "Đổi tên file ngắn gọn, tránh ký tự đặc biệt rồi thử lại."],
  meta_path: ["Không thể tải vào thư mục hệ thống của FileSync",
    "Hãy thả file vào vị trí khác trong thư mục đồng bộ."],
  dial_failed: ["Không nối được tới máy kia",
    "Kiểm tra: máy kia đang mở FileSync, hai máy đang cùng một mạng Wi-Fi/LAN, và địa chỉ đã gõ đúng chưa."],
  pair_code: ["Mã ghép nối không đúng hoặc đã hết hạn",
    "Tạo mã mới ở máy kia rồi nhập lại. Mã chỉ dùng được trong 5 phút."],
  internal: ["Có lỗi khi đồng bộ",
    "FileSync sẽ tự thử lại. Nếu lỗi lặp lại nhiều lần, hãy tắt và mở lại FileSync trên cả hai máy."],
};
function explainError(code, raw) {
  if (code && ERROR_TEXT[code]) {
    const t = ERROR_TEXT[code];
    return { title: t[0], hint: t[1] };
  }
  const t = ERROR_TEXT.internal;
  return { title: t[0], hint: t[1] };
}
// errorHTML renders một lỗi đầy đủ: tiêu đề + gợi ý + chi tiết
// kỹ thuật thu gọn. raw/code là dữ liệu từ xa tiềm ẩn — luôn esc.
function errorHTML(code, raw) {
  const e = explainError(code, raw);
  let h = "<div class='err-box'><div class='err-title'>" + esc(e.title) + "</div>" +
    "<div class='err-hint'>" + esc(e.hint) + "</div>";
  if (raw || code) {
    h += "<details class='tech'><summary>Chi tiết kỹ thuật</summary><pre>" +
      esc((code ? "[" + code + "] " : "") + (raw || "")) + "</pre></details>";
  }
  return h + "</div>";
}
// classifyDialError: lỗi dial ghép nối không có mã ổn định ở
// tầng pairing; phân loại theo nội dung message an toàn của nó.
function classifyDialError(err) {
  const raw = (err && err.message) || "";
  if (/proof invalid|exactly 6 digits|expired|unknown session/i.test(raw)) return "pair_code";
  return "dial_failed";
}

/* ---------- Trạng thái + Trang chính ---------- */
function setPill(cls, text) {
  const p = $("conn-state");
  p.className = "pill " + cls;
  p.innerHTML = "<span class='dot'></span>" + esc(text);
}

async function loadStatus() {
  let st;
  try { st = await api("/api/status"); }
  catch (e) {
    setPill("pill-red", "Mất kết nối tới daemon");
    return;
  }
  FS.status = st;
  if (st.paused) setPill("pill-amber", "Đã tạm dừng đồng bộ");
  else if (st.state === "running") setPill("pill-green", "Đang đồng bộ");
  else setPill("pill-gray", st.state);
  $("header-device").textContent = st.device_name + " · " + st.device_id;
  $("stat-state").textContent = st.paused ? "Tạm dừng" : (st.state === "running" ? "Đang chạy" : st.state);
  $("stat-files").textContent = st.indexed_files;
  $("stat-sent").textContent = fmtBytes(st.bytes_sent);
  $("stat-recv").textContent = fmtBytes(st.bytes_received);
  $("stat-rate").textContent = fmtBytes(st.rate_bps || 0) + "/s";
  $("btn-pause").disabled = !!st.paused;
  $("btn-resume").disabled = !st.paused;
  const ownAddr = (st.lan_addrs && st.lan_addrs[0]) || st.listen_addr || "";
  $("overview-body").innerHTML =
    "<dl class='kv'>" +
    kv("Tên thiết bị", esc(st.device_name)) +
    kv("Device ID", "<span class='mono'>" + esc(st.device_id) + "</span>") +
    kv("Địa chỉ của máy này", "<span class='mono'>" + esc(ownAddr) + "</span>") +
    kv("Thư mục đồng bộ", esc(st.sync_dir)) +
    kv("Số vòng đã đồng bộ", st.rounds) +
    "</dl><div class='hint'>Hình và dãy từ này là “căn cước” của máy bạn — máy kia sẽ thấy y hệt khi ghép nối:</div>" +
    identityCardHTML(st.fingerprint, 56);
  const warn = $("skip-warning");
  if (st.skipped_version_archives > 0) {
    const last = st.last_version_skip;
    warn.textContent = "⚠ " + st.skipped_version_archives + " lần ghi đè file lớn đã KHÔNG lưu bản cũ vào lịch sử phiên bản (file vượt hạn mức lưu trữ)" +
      (last ? ": gần nhất là " + last.path + " (" + fmtBytes(last.size) + ")." : ".");
    warn.classList.remove("hidden");
  } else warn.classList.add("hidden");
  renderTransfers(st);
  renderRecentErrors(st);
  renderOfflineBanner();
  renderHomeFiles();
  for (const fn of FS.onStatus) { try { fn(st); } catch (e) { /* hook lỗi không được làm chết poll */ } }
}

function renderOfflineBanner() {
  const el = $("offline-banner");
  const off = FS.peers.filter((p) => !p.revoked && !p.connected);
  if (!off.length) { el.classList.add("hidden"); el.innerHTML = ""; return; }
  el.innerHTML = "Máy <strong>" + esc(off[0].name || "kia") + "</strong> đang không trực tuyến — " +
    "file mới sẽ tự chuyển khi máy đó mở lại.";
  el.classList.remove("hidden");
}

function progressHTML(done, total, state) {
  const pct = total > 0 ? Math.min(100, Math.round(100 * done / total)) : 0;
  // cls là hằng nội bộ (không phải dữ liệu ngoài) — chọn nguyên
  // chuỗi literal để bộ quét XSS của job 006 không phải nới lệ.
  const bar = state === "failed" ? "<i class='failed'" : (pct >= 100 ? "<i class='done'" : "<i");
  return "<div class='progress'>" + bar + " style='width:" + pct + "%'></i></div>" +
    "<div class='hint'>" + pct + "% · " + fmtBytes(done) + " / " + fmtBytes(total) + "</div>";
}

function renderTransfers(st) {
  const sends = st.sends || [];
  const queue = st.queue || [];
  // Rỗng -> panel thu gọn 1 dòng (.idle trong style.css).
  $("transfers-panel").classList.toggle("idle", sends.length + queue.length === 0);
  $("transfers-empty").classList.toggle("hidden", sends.length + queue.length > 0);
  $("sending-title").classList.toggle("hidden", !sends.length);
  $("receiving-title").classList.toggle("hidden", !queue.length);
  $("sending-list").innerHTML = sends.map((s) =>
    "<div class='transfer-item'><div class='transfer-head'>" +
    "<span class='transfer-name' title='" + esc(s.Path) + "'>" + esc(baseName(s.Path)) + "</span>" +
    "<span class='transfer-peer'>tới máy " + esc(FS.peerName(s.PeerID)) + "</span></div>" +
    progressHTML(s.BytesDone, s.BytesTotal, s.State) + "</div>"
  ).join("");
  $("receiving-list").innerHTML = queue.map((q) => {
    let h = "<div class='transfer-item'><div class='transfer-head'>" +
      "<span class='transfer-name' title='" + esc(q.Path) + "'>" + esc(baseName(q.Path)) + "</span>" +
      "<span class='transfer-peer'>từ máy " + esc(FS.peerName(q.PeerID)) + "</span></div>";
    if (q.State === "failed") {
      h += errorHTML(q.Err, q.Path);
    } else {
      if (q.State === "deferred") {
        h += "<div class='hint'>Đang chờ (thiếu dung lượng hoặc máy bận), FileSync sẽ tự thử lại.</div>";
      }
      h += progressHTML(q.BytesDone, q.BytesTotal, q.State);
    }
    return h + "</div>";
  }).join("");
}

function renderRecentErrors(st) {
  const errs = $("recent-errors");
  const list = st.recent_errors || [];
  if (!list.length) { errs.innerHTML = ""; return; }
  errs.innerHTML = "<h3>Lỗi gần đây</h3>" + list.map((e) =>
    errorHTML(e.Code, (e.Path ? e.Path + " — " : "") + (e.Message || ""))).join("");
}

function renderHomeFiles() {
  const el = $("home-files");
  if (!el) return;
  const sending = new Set((FS.status && FS.status.sends || []).map((s) => s.Path));
  const receiving = new Set((FS.status && FS.status.queue || []).map((q) => q.Path));
  if (!FS.files.length) {
    el.innerHTML = "<p class='hint'>Chưa có file nào. Hãy kéo file vào khung phía trên, hoặc copy file vào thư mục đồng bộ.</p>";
    return;
  }
  el.innerHTML = FS.files.map((f) => {
    let tag = "<span class='tag'>Trên máy này</span>";
    if (sending.has(f.path)) tag = "<span class='tag tag-send'>Đang gửi</span>";
    else if (receiving.has(f.path)) tag = "<span class='tag tag-recv'>Đang nhận</span>";
    return "<div class='file-row'><span class='file-name'>" + esc(f.path) + "</span>" +
      "<span>" + tag + " <span class='hint'>" + fmtBytes(f.size) + "</span></span></div>";
  }).join("");
}

async function loadFiles() {
  try {
    FS.files = await api("/api/files");
    $("files-list").innerHTML = FS.files.map((f) => "<option value='" + esc(f.path) + "'>").join("");
    renderHomeFiles();
  } catch (e) { /* lần poll sau thử lại */ }
}
FS.loadFiles = loadFiles;

/* ---------- Máy đã ghép đôi ---------- */
async function loadPeers() {
  try { FS.peers = await api("/api/peers"); }
  catch (e) { return; }
  renderPeers();
  renderOfflineBanner();
  for (const fn of FS.onPeers) { try { fn(FS.peers); } catch (e) { /* như trên */ } }
}

function renderPeers() {
  const el = $("peers-body");
  if (!FS.peers.length) {
    el.innerHTML = "<div class='card empty'>Chưa ghép đôi với máy nào.<br>Hãy sang thẻ “Ghép đôi” để bắt đầu.</div>";
    return;
  }
  el.innerHTML = FS.peers.map((p) => {
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
  el.querySelectorAll("[data-revoke]").forEach((btn) => {
    btn.addEventListener("click", () => withBusy(btn, async () => {
      const id = btn.getAttribute("data-revoke");
      if (!confirm("Thu hồi quyền của máy “" + btn.getAttribute("data-name") + "”?\nMáy đó sẽ bị ngắt kết nối ngay và không đồng bộ được nữa. Muốn dùng lại phải ghép đôi lại từ đầu.")) return;
      try { await post("/api/peers/revoke", { device_id: id }); toast("Đã thu hồi quyền của máy."); loadPeers(); }
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
    FS.activeTab = btn.getAttribute("data-tab");
    $("tab-" + FS.activeTab).classList.add("active");
    if (FS.activeTab === "peers") renderPeers();
    if (FS.activeTab === "conflicts") loadConflicts().catch((e) => toast(e.message, "err"));
    if (FS.activeTab === "history") loadFiles().catch(() => {});
    if (FS.activeTab === "home") { loadFiles().catch(() => {}); loadStatus().catch(() => {}); }
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
$("btn-history").addEventListener("click", (ev) => withBusy(ev.currentTarget, loadHistory));

loadStatus().catch(() => {});
loadPeers().catch(() => {});
loadFiles().catch(() => {});
loadConflicts().catch(() => {});
setInterval(() => { loadStatus().catch(() => {}); }, 1000);
setInterval(() => { loadPeers().catch(() => {}); }, 3000);
setInterval(() => { if (FS.activeTab === "home") loadFiles().catch(() => {}); }, 5000);
