"use strict";
// Kéo–thả / chọn file để gửi (job 009, spec F5/D4). Mỗi file
// một XHR POST /api/upload?path=<đường dẫn tương đối>, body là
// chính File (trình duyệt stream từ đĩa, không nạp RAM), tối đa
// 2 file song song. File lên tới daemon là vào thư mục đồng bộ —
// từ đó engine tự chuyển sang máy kia như mọi file cục bộ.

const uploadQueue = [];   // {id, file, rel, state, pct, errCode, errRaw, xhr}
let uploadActive = 0;
let uploadSeq = 0;

function enqueueFiles(items) {
  for (const it of items) {
    uploadQueue.push({
      id: ++uploadSeq, file: it.file, rel: it.rel,
      state: "queued", pct: 0, errCode: "", errRaw: "",
    });
  }
  renderUploadQueue();
  pumpUploads();
}

function pumpUploads() {
  while (uploadActive < 2) {
    const item = uploadQueue.find((x) => x.state === "queued");
    if (!item) return;
    startUpload(item);
  }
}

function startUpload(item) {
  item.state = "uploading";
  uploadActive++;
  const xhr = new XMLHttpRequest();
  item.xhr = xhr;
  xhr.open("POST", "/api/upload?path=" + encodeURIComponent(item.rel));
  xhr.upload.onprogress = (ev) => {
    if (ev.lengthComputable && ev.total > 0) {
      item.pct = Math.round(100 * ev.loaded / ev.total);
      renderUploadQueue();
    }
  };
  xhr.onload = () => {
    uploadActive--;
    let body = null;
    try { body = JSON.parse(xhr.responseText); } catch (e) { /* lỗi tĩnh */ }
    if (xhr.status >= 200 && xhr.status < 300) {
      item.state = "done";
      item.pct = 100;
      if (window.FS && FS.loadFiles) FS.loadFiles();
    } else {
      item.state = "error";
      item.errCode = (body && body.code) || "internal";
      item.errRaw = (body && body.error) || ("HTTP " + xhr.status);
    }
    renderUploadQueue();
    pumpUploads();
  };
  xhr.onerror = () => {
    uploadActive--;
    item.state = "error";
    item.errCode = "internal";
    item.errRaw = "network error";
    renderUploadQueue();
    pumpUploads();
  };
  xhr.send(item.file);
}

function renderUploadQueue() {
  const box = $("upload-queue");
  if (!uploadQueue.length) { box.innerHTML = ""; return; }
  box.innerHTML = uploadQueue.map((it) => {
    let h = "<div class='upload-item'><div class='upload-head'>" +
      "<span class='upload-name'>" + esc(it.rel) + "</span>" +
      "<span class='hint'>" + fmtBytes(it.file.size) + "</span></div>";
    if (it.state === "queued") {
      h += "<div class='hint'>Đang chờ tải lên…</div>";
    } else if (it.state === "uploading") {
      h += "<div class='progress'><i style='width:" + it.pct + "%'></i></div>" +
        "<div class='hint'>Đang tải lên… " + it.pct + "%</div>";
    } else if (it.state === "done") {
      h += "<div class='progress'><i class='done' style='width:100%'></i></div>" +
        "<div class='hint'>Đã đưa vào thư mục đồng bộ — FileSync sẽ tự chuyển sang máy kia.</div>";
    } else {
      h += "<div class='progress'><i class='failed' style='width:" + it.pct + "%'></i></div>" +
        errorHTML(it.errCode, it.errRaw) +
        "<div class='row'><button class='btn' data-retry='" + it.id + "'>Thử lại</button></div>";
    }
    return h + "</div>";
  }).join("");
  box.querySelectorAll("[data-retry]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const it = uploadQueue.find((x) => x.id === Number(btn.getAttribute("data-retry")));
      if (it) { it.state = "queued"; it.pct = 0; renderUploadQueue(); pumpUploads(); }
    });
  });
}

/* ---------- nguồn file: nút chọn ---------- */
$("btn-pick").addEventListener("click", () => $("file-input").click());
$("btn-pick-dir").addEventListener("click", () => $("dir-input").click());
$("file-input").addEventListener("change", (ev) => {
  const items = [...ev.target.files].map((f) => ({ file: f, rel: f.name }));
  ev.target.value = "";
  enqueueFiles(items);
});
$("dir-input").addEventListener("change", (ev) => {
  const items = [...ev.target.files].map((f) => ({ file: f, rel: f.webkitRelativePath || f.name }));
  ev.target.value = "";
  enqueueFiles(items);
});

/* ---------- nguồn file: kéo–thả ---------- */
const dropzone = $("dropzone");
for (const evt of ["dragenter", "dragover"]) {
  dropzone.addEventListener(evt, (ev) => { ev.preventDefault(); dropzone.classList.add("dragover"); });
}
for (const evt of ["dragleave", "drop"]) {
  dropzone.addEventListener(evt, (ev) => { ev.preventDefault(); dropzone.classList.remove("dragover"); });
}
dropzone.addEventListener("drop", async (ev) => {
  const dt = ev.dataTransfer;
  const items = [];
  if (dt.items && dt.items.length && dt.items[0].webkitGetAsEntry) {
    const entries = [...dt.items]
      .filter((i) => i.kind === "file")
      .map((i) => i.webkitGetAsEntry())
      .filter(Boolean);
    for (const entry of entries) {
      await collectEntry(entry, "", items);
    }
  } else if (dt.files) {
    for (const f of dt.files) items.push({ file: f, rel: f.name });
  }
  if (items.length) enqueueFiles(items);
});

// collectEntry duyệt đệ quy một FileSystemEntry (file hoặc thư
// mục) thành danh sách {file, rel} giữ nguyên cấu trúc thư mục.
function collectEntry(entry, prefix, out) {
  return new Promise((resolve) => {
    if (entry.isFile) {
      entry.file((f) => { out.push({ file: f, rel: prefix + entry.name }); resolve(); }, () => resolve());
    } else if (entry.isDirectory) {
      const reader = entry.createReader();
      const readBatch = () => {
        reader.readEntries(async (batch) => {
          if (!batch.length) { resolve(); return; }
          for (const child of batch) await collectEntry(child, prefix + entry.name + "/", out);
          readBatch();
        }, () => resolve());
      };
      readBatch();
    } else resolve();
  });
}
