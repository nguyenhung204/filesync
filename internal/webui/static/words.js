"use strict";
// "Thẻ căn cước" của fingerprint (job 009 vòng 3): chuỗi hex 64 ký
// tự không ai đọc nổi, nên fingerprint được trình bày lại thành
// dãy 8 từ tiếng Việt + hình họa tiết (identicon) — cả hai đều suy
// ra xác định từ chính các byte của fingerprint nên hai máy luôn
// cho ra y hệt nhau. Bảng từ do script sinh (factory/jobs/009-ux/
// round-3/make_fpwords.py), khớp byte-for-byte với bản Go trong
// internal/cli (fpWordsTable) qua test vector dùng chung.
// Hàm này chỉ đổi CÁCH TRÌNH BÀY — duyệt ghép nối vẫn thủ công.

// FP_WORDS_BEGIN
const FP_WORDS = [
  "một", "hai", "ba", "bốn",
  "năm", "sáu", "bảy", "tám",
  "chín", "mười", "mèo", "chó",
  "gà", "vịt", "bò", "dê",
  "ngựa", "thỏ", "gấu", "hổ",
  "khỉ", "voi", "chim", "ếch",
  "rắn", "rùa", "kiến", "ong",
  "cua", "tôm", "trâu", "nai",
  "sóc", "cáo", "hươu", "công",
  "quạ", "nhện", "ốc", "mực",
  "ngỗng", "heo", "sói", "cừu",
  "chuột", "bướm", "cò", "sếu",
  "hạc", "én", "chồn", "lươn",
  "sứa", "hàu", "ghẹ", "tép",
  "diều", "sẻ", "ó", "lóc",
  "rô", "chép", "trắm", "vạc",
  "cuốc", "táo", "lê", "đào",
  "mận", "xoài", "cam", "quýt",
  "bưởi", "chanh", "chuối", "dừa",
  "mít", "nhãn", "vải", "hồng",
  "sim", "ổi", "cóc", "me",
  "khế", "sung", "na", "tre",
  "trúc", "sen", "huệ", "lan",
  "cúc", "mai", "quế", "hồi",
  "gừng", "nghệ", "sả", "tỏi",
  "hành", "tiêu", "gạo", "ngô",
  "khoai", "sắn", "đậu", "lạc",
  "vừng", "đường", "mật", "cơm",
  "phở", "bún", "cháo", "canh",
  "thịt", "trứng", "bánh", "kẹo",
  "trà", "rượu", "bia", "miến",
  "trời", "mây", "mưa", "nắng",
  "gió", "bão", "sấm", "chớp",
  "sương", "tuyết", "đá", "núi",
  "sông", "suối", "biển", "cát",
  "đồng", "đèo", "dốc", "thác",
  "vịnh", "ao", "đầm", "vực",
  "sao", "lửa", "khói", "than",
  "củi", "rơm", "lá", "gốc",
  "mầm", "nụ", "xuân", "hạ",
  "thu", "sớm", "trưa", "chiều",
  "đêm", "cầu", "đập", "mương",
  "rạch", "thôn", "ấp", "bơi",
  "chạy", "leo", "khóc", "cười",
  "ngủ", "mơ", "yêu", "nhớ",
  "quên", "tủ", "khóa", "chuông",
  "đèn", "nến", "gương", "lược",
  "búa", "thước", "bút", "giấy",
  "sách", "cặp", "mũ", "quần",
  "dép", "tất", "khăn", "túi",
  "ví", "kính", "đàn", "nồi",
  "bát", "thìa", "ấm", "chén",
  "ly", "bình", "lò", "chum",
  "thúng", "nia", "gối", "quạt",
  "xô", "neo", "chèo", "lưới",
  "kim", "chỉ", "nút", "phao",
  "thuyền", "tàu", "kiếm", "cung",
  "khiên", "tên", "phà", "bè",
  "tay", "mũi", "tai", "tóc",
  "lưng", "tim", "móng", "máu",
  "má", "môi", "đuôi", "vuốt",
  "tổ", "nhà", "hẻm", "thềm",
  "bếp", "phòng", "kho", "bến",
  "tiệm", "nâu", "thấp", "rộng",
];
// FP_WORDS_END

// fpBytes bóc chuỗi fingerprint (hex viết hoa, cách nhau bằng
// khoảng trắng theo nhóm 4) thành mảng byte; chuỗi lạ -> mảng ngắn.
function fpBytes(fp) {
  const hex = String(fp || "").replace(/[^0-9A-Fa-f]/g, "");
  const out = [];
  for (let i = 0; i + 2 <= hex.length; i += 2) {
    out.push(parseInt(hex.slice(i, i + 2), 16));
  }
  return out;
}

// fpWords: 8 từ đối chiếu từ 8 byte đầu của fingerprint.
function fpWords(fp) {
  const b = fpBytes(fp);
  if (b.length < 8) return [];
  const words = [];
  for (let i = 0; i < 8; i++) words.push(FP_WORDS[b[i]]);
  return words;
}

// 8 màu họa tiết thân thiện, cố ý tránh các màu trạng thái
// (ok/warn/err/info) để không ai hiểu nhầm hình là báo trạng thái.
const FP_ID_COLORS = [
  "#2563eb", "#7c3aed", "#db2777", "#ea580c",
  "#0891b2", "#4f46e5", "#9333ea", "#0d9488",
];

// fpIdenticonSVG: lưới 5x5 đối xứng trái-phải. 15 ô nửa trái lấy
// từ 15 byte sau dãy từ (bit cao của mỗi byte), nửa phải là gương;
// màu lấy từ 3 byte tiếp theo. Cùng fingerprint -> cùng hình.
function fpIdenticonSVG(fp, size) {
  const b = fpBytes(fp);
  if (b.length < 26) return "";
  const color = FP_ID_COLORS[(b[23] + b[24] + b[25]) % FP_ID_COLORS.length];
  let rects = "";
  for (let r = 0; r < 5; r++) {
    for (let c = 0; c < 3; c++) {
      if (b[8 + r * 3 + c] & 0x80) {
        rects += "<rect x='" + c + "' y='" + r + "' width='1' height='1'/>";
        if (c !== 2) {
          rects += "<rect x='" + (4 - c) + "' y='" + r + "' width='1' height='1'/>";
        }
      }
    }
  }
  return "<svg class='identicon-svg' viewBox='0 0 5 5' width='" + size +
    "' height='" + size + "' role='img' aria-label='Hình căn cước'>" +
    "<rect width='5' height='5' fill='#f3f3f7'/><g fill='" + color + "'>" +
    rects + "</g></svg>";
}

// identityCardHTML render trọn "thẻ căn cước": hình + dãy từ đánh
// số (đọc qua điện thoại theo thứ tự), chuỗi hex lui vào mục
// "Chi tiết kỹ thuật". Fingerprint lạ/ngắn -> quay về hex thuần.
function identityCardHTML(fp, size) {
  const words = fpWords(fp);
  if (words.length !== 8) return fingerprintHTML(fp);
  const cells = words.map((w, i) =>
    "<div class='fp-word'><span class='fp-num'>" + (i + 1) + "</span>" +
    "<span class='fp-word-text'>" + esc(w) + "</span></div>").join("");
  return "<div class='idcard'>" + fpIdenticonSVG(fp, size) +
    "<div class='fp-words'>" + cells + "</div></div>" +
    "<details class='tech'><summary>Chi tiết kỹ thuật</summary>" +
    fingerprintHTML(fp) + "</details>";
}

// Cho phép kiểm chứng bằng node (harness Playwright) — trình duyệt
// bỏ qua nhánh này.
if (typeof module !== "undefined" && module.exports) {
  module.exports = { FP_WORDS, FP_ID_COLORS, fpBytes, fpWords, fpIdenticonSVG };
}
