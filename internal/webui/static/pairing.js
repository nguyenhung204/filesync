"use strict";
// Ghép nối (job 009): wizard lần đầu + tab Ghép đôi dùng chung
// renderer. Hai khối độc lập: (A) hiện mã của máy này, (B) nhập
// mã của máy kia, cộng mục "Máy ở gần đây" từ mDNS (addendum).
// An toàn giữ nguyên job 006: fingerprint luôn hiển thị và
// người dùng tự bấm Duyệt ở cả hai máy — không tự duyệt bao giờ.

let pairCodeExpiresAt = 0; // unix giây, dùng chung cả 2 instance

/* ---------- quyết định wizard (D6) ---------- */
let wizardWasShown = false;
function wizardNeeded() {
  const active = FS.peers.filter((p) => !p.revoked).length;
  return active === 0 && !sessionStorage.getItem("fs_skip_wizard");
}
function evaluateWizard() {
  const show = wizardNeeded();
  const wiz = $("wizard");
  const app = $("app");
  // Vừa ghép xong trong lúc đang ở wizard (peers đi 0 -> >=1):
  // chúc mừng rồi tự chuyển sang màn hình chính (F4).
  if (!show && wizardWasShown && FS.peers.some((p) => !p.revoked)) {
    const p = FS.peers.find((x) => !x.revoked);
    toast("Đã ghép nối xong với máy " + (p ? p.name : "kia") + "!");
  }
  wizardWasShown = show;
  wiz.classList.toggle("hidden", !show);
  app.classList.toggle("hidden", show);
}
$("wizard-skip").addEventListener("click", (ev) => {
  ev.preventDefault();
  sessionStorage.setItem("fs_skip_wizard", "1");
  evaluateWizard();
});

/* ---------- địa chỉ của máy này ---------- */
function ownAddrText() {
  const st = FS.status;
  if (!st) return "…";
  return (st.lan_addrs && st.lan_addrs[0]) || st.listen_addr || "…";
}
function renderOwnAddr() {
  for (const id of ["own-addr", "own-addr-w"]) {
    const el = $(id);
    if (el) el.textContent = ownAddrText();
  }
}

/* ---------- máy ở gần đây (addendum) ---------- */
function agoText(nano) {
  if (!nano) return "";
  const s = Math.max(0, Math.round((Date.now() - nano / 1e6) / 1000));
  if (s < 5) return "vừa thấy xong";
  if (s < 60) return "thấy cách đây " + s + " giây";
  return "thấy cách đây " + Math.round(s / 60) + " phút";
}
function renderNearby() {
  const list = (FS.status && FS.status.discovered) || [];
  const html = list.map((d, i) =>
    "<button class='nearby-item' data-nearby='" + i + "'>" +
    "<span><span class='nearby-name'>" + esc(d.name || "Máy FileSync") + "</span><br>" +
    "<span class='nearby-addr mono'>" + esc(d.addr) + "</span></span>" +
    "<span class='hint'>" + esc(agoText(d.last_seen)) + "</span></button>"
  ).join("");
  for (const id of ["nearby-list", "nearby-list-w"]) {
    const el = $(id);
    if (el) el.innerHTML = html;
  }
  for (const id of ["nearby-empty", "nearby-empty-w"]) {
    const el = $(id);
    if (el) el.classList.toggle("hidden", list.length > 0);
  }
  document.querySelectorAll("[data-nearby]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const d = list[Number(btn.getAttribute("data-nearby"))];
      if (!d) return;
      for (const id of ["dial-addr", "dial-addr-w"]) {
        const el = $(id);
        if (el) el.value = d.addr;
      }
      toast("Đã điền địa chỉ của máy " + (d.name || "kia") + ". Giờ nhập mã 6 số của máy đó rồi bấm Ghép nối.");
      const code = wizardNeeded() ? $("dial-code-w") : $("dial-code");
      if (code) code.focus();
    });
  });
}

/* ---------- khối (A): hiện mã ---------- */
async function createCode(btn) {
  await withBusy(btn, async () => {
    try {
      const r = await post("/api/pair/code");
      pairCodeExpiresAt = r.expires_at;
      for (const pfx of ["", "-w"]) {
        const disp = $("code-display" + pfx);
        if (!disp) continue;
        disp.classList.remove("hidden");
        $("code-value" + pfx).textContent = r.code.split("").join(" ");
      }
      renderOwnAddr();
      tickCodeExpiry();
      toast("Đã tạo mã ghép đôi. Đọc địa chỉ và mã cho máy kia nhập vào.");
    } catch (e) { toast(e.message, "err"); }
  });
}
function tickCodeExpiry() {
  if (!pairCodeExpiresAt) return;
  const left = pairCodeExpiresAt - Math.floor(Date.now() / 1000);
  const text = left > 0
    ? "Mã còn hiệu lực " + Math.floor(left / 60) + ":" + String(left % 60).padStart(2, "0")
    : "Mã đã hết hạn — hãy tạo mã mới.";
  for (const id of ["code-expiry", "code-expiry-w"]) {
    const el = $(id);
    if (el) el.textContent = text;
  }
}
setInterval(tickCodeExpiry, 1000);
$("btn-code").addEventListener("click", (ev) => createCode(ev.currentTarget));
$("btn-code-w").addEventListener("click", (ev) => createCode(ev.currentTarget));

/* ---------- khối (B): nhập mã / dial ---------- */
async function dial(btn, pfx) {
  await withBusy(btn, async () => {
    const addr = $("dial-addr" + pfx).value.trim();
    const code = $("dial-code" + pfx).value.replace(/\s+/g, "");
    const errBox = $("dial-err" + pfx);
    errBox.innerHTML = "";
    if (!addr || !code) {
      errBox.innerHTML = errorHTML("dial_failed", "chưa nhập đủ địa chỉ và mã 6 số");
      return;
    }
    try {
      await post("/api/pair/dial", { addr: addr, code: code });
      const msg = $("dial-msg");
      if (msg) msg.textContent = "";
      toast("Đã nối tới máy kia. Hãy đối chiếu fingerprint trong mục chờ duyệt rồi bấm Duyệt ở cả hai máy.");
      loadPending();
    } catch (e) {
      errBox.innerHTML = errorHTML(classifyDialError(e), e.message);
    }
  });
}
$("btn-dial").addEventListener("click", (ev) => dial(ev.currentTarget, ""));
$("btn-dial-w").addEventListener("click", (ev) => dial(ev.currentTarget, "-w"));

/* ---------- yêu cầu chờ duyệt ---------- */
function pendingCardHTML(p) {
  return "<div class='card pending-card'><div class='peer-name'>⏳ " + esc(p.name) +
    " <span class='hint'>đang xin ghép đôi</span></div>" +
    "<div class='mono'>Device ID: " + esc(p.device_id) + "</div>" +
    "<div class='hint'>Fingerprint của máy kia — đối chiếu kỹ từng nhóm với màn hình của máy đó. " +
    "Chỉ bấm Duyệt khi khớp hoàn toàn:</div>" +
    fingerprintHTML(p.fingerprint) +
    "<div class='actions'>" +
    "<button class='btn btn-primary btn-big' data-confirm='" + esc(p.session_id) + "'>✓ Duyệt — fingerprint khớp</button>" +
    "<button class='btn btn-big' data-reject='" + esc(p.session_id) + "'>✕ Từ chối</button>" +
    "</div></div>";
}
async function loadPending() {
  let pending;
  try { pending = await api("/api/pending"); }
  catch (e) { return; }
  const badge = $("pending-count");
  badge.textContent = pending.length || "";
  badge.classList.toggle("hidden", !pending.length);
  const html = pending.length ? pending.map(pendingCardHTML).join("") : "Không có yêu cầu nào.";
  $("pending-body").innerHTML = html;
  $("pending-body-w").innerHTML = html;
  $("wizard-pending").classList.toggle("hidden", !pending.length);
  document.querySelectorAll("[data-confirm]").forEach((btn) => {
    btn.addEventListener("click", () => withBusy(btn, async () => {
      try {
        await post("/api/pair/confirm", { session_id: btn.getAttribute("data-confirm") });
        toast("Đã duyệt ở máy này — ghép đôi hoàn tất khi máy kia cũng duyệt.");
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

/* ---------- nối vào nhịp poll của app.js ---------- */
FS.onStatus.push(() => { renderOwnAddr(); renderNearby(); });
FS.onPeers.push(() => { evaluateWizard(); });
evaluateWizard();
renderNearby();
loadPending().catch(() => {});
setInterval(() => { loadPending().catch(() => {}); }, 3000);
