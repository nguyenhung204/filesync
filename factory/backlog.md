# Backlog dọn dẹp (NIT từ review — không chặn merge)

## GATE từ approver job 001 — ĐÃ ĐÓNG bởi job 002c (merge 2026-10-11)
- [x] validate: reject UTF-8 không hợp lệ + reserved names CONIN$/CONOUT$ (spec job 001 đã cập nhật).
- [x] chunk: Bitmap.UnmarshalBinary chống tràn số.

## NIT thường — ĐÃ XONG ở job 002c
- [x] validate: ValidateSegment NFC + Error.Path gốc; vclock canonical khoá rỗng; config Load validate; ignore reject segment rỗng.

## Giới hạn đã biết (từ approver job 002 — đưa vào README ở job 004)
- [ ] Race microsecond check→rename/hash còn lại (cần API no-follow theo descriptor mới đóng triệt để) — job 003/004 không coi là đã đóng hẳn.
- [ ] PathWithinRoot: hợp đồng đầu vào — không dùng một mình làm cổng mở-ghi; job dọn dẹp thêm doc precondition hoặc reject segment "..".
- [ ] RequireSpace là check point-in-time — in-flight budget thuộc job 003/004 (cộng dồn transfer đang chạy).
- [ ] Rescan mặc định code = 30s vs prompt gốc 60s — đồng bộ tài liệu ở job 004.
- [ ] Job 004: khoá chống 2 daemon cùng mở 1 store (single-instance lock).
- [ ] README giới hạn đã biết (job 004): mDNS thật trên LAN chưa test được trong môi trường dev (test env-gated FILESYNC_TEST_MDNS=1); restart daemon giữa lúc pairing làm mất cửa mã (codeKey chỉ trong RAM — fail-safe có chủ đích).
- [ ] Job 004: daemon dùng transport.DefaultOptions() (không tự đặt MDNS=false bằng bool zero).
- [ ] Job 004 lưu ý từ approver job 003: chữ ký DialPair(ctx, tm, addr, code); phía acceptor KHÔNG lưu Addresses của initiator — phiên sync phải chạy 2 chiều trên cùng kết nối đã mở, không trông chờ dial-back theo địa chỉ nguồn (cổng ephemeral).
- [ ] Phòng thủ chiều sâu: DecompressChunkPayload tự kiểm trần MaxGzipPayloadBytes + khoảng Size khi caller bỏ qua Validate (approver job 004a).
- [ ] ƯU TIÊN CAO — job 004b: CLI `history` + `restore` (tầng engine đã có, thiếu bề mặt CLI) + bổ sung 1 dòng README về nợ fsync đường pull (điều kiện của approver job 004).
- [ ] Phase 2: store-op gộp "complete transfer" để giảm ~7 fsync/file ở đường pull (approver job 004).
- [ ] Phase 2: cân nhắc hạ trần entry index 1M (spike heap ~471MiB khi bị flood — security round 2 job 004).
- [ ] NIT: `restore --version=` (rỗng) nên báo lỗi tham số thay vì lặng lẽ về bản mới nhất (approver job 004b).
- [x] Job 004b: CLI history/restore + dòng README fsync — XONG (merge 2026-10-11).
- [x] ] ƯU TIÊN — job 005b vá bộ cài Windows (phát hiện khi user cài thật 2026-10-11): install-service.ps1 chứa em-dash UTF-8 không BOM → Windows PowerShell 5.1 đọc ANSI làm hỏng parser (byte 0x94 thành dấu ngoặc kép), service không được tạo (lỗi 1060) mà .bat vẫn in "cài đặt xong". Việc: scripts .ps1 về ASCII thuần + kiểm tra exit code của sc.exe start + chờ/xác nhận service RUNNING; cai-dat .bat kiểm tra service tồn tại sau khi nâng quyền, lỗi thì báo rõ thay vì in thành công vô điều kiện. Daemon chạy tiền cảnh trên Windows thật đã kiểm chứng OK. — XONG 2026-10-11 (job 005b, merge 649423d, Release v0.1.0 đã publish).
- [x] Job 006 Web UI loopback + job 007 FileSync-Setup.exe — XONG 2026-10-11, Release v0.2.0 đã publish (Setup 1 file double-click, UI tiếng Việt + Playwright verified, XSS + PS-injection đã đóng qua review).

## Sau v0.3.0 (2026-10-12, từ job 009 decision + sự cố Windows thật)
- Backfill tên peer từ Hello khi kết nối (tên peer ghép từ bản ≤0.2.0 giữ rỗng vĩnh viễn).
- Gọt ký tự điều khiển cho tên peer CŨ đã lưu trong store tại điểm đọc (store mới đã sạch từ nguồn; dữ liệu cũ chỉ hở lý thuyết ở CLI in %s).
- Firewall rule nên đọc cổng thật từ config thay vì ghim 22000/5353 mặc định.
- Goroutine publish ui_addr của daemon nên lặp tới khi ctx huỷ (hiện publish bất đồng bộ — test từng flake vì đọc 1 lần).
- Thông báo cho file bị skip vì tên cấm Windows (hiện bị loại lặng ở vòng quét).
- daemon.go đã tách watch.go; run_windows.go đúng trần 500/500 — thêm code phải tách file trước.
- Kiểm chứng Windows/LAN thật cho v0.3.0 (checklist 8 mục trong decision job 009): service RUNNING, tự mở UI, console có dấu trên codepage thật, mDNS có dữ liệu trên LAN thật, firewall, icon shortcut, nâng cấp giữ dữ liệu, kéo-thả + tiến trình 2 phía.
- Job 010 (XONG 2026-10-12): service đọc sai thư mục cấu hình vì cờ --config-dir sau `serve` bị CLI bỏ qua → service chạy bằng config systemprofile; đã sửa ở CLI (serve nhận cờ trong args của chính nó, chữa cả đăng ký cũ) + test đầu-cuối theo đúng dạng binPath; Release v0.3.1. Máy ADMIN của user đã chữa tay bằng registry trước đó nên tuyên bố "chữa đăng ký dạng cũ" cần xác nhận thêm trên máy còn giữ binPath nguyên dạng (Maxsys khi cài v0.3.1).
- Job 011 (XONG 2026-10-12): bỏ footer Web UI theo yêu cầu user; đi chung v0.3.1.
- Thư mục cấu hình mồ côi ở systemprofile trên các máy đã cài v0.3.0 (do job 010 mô tả) — vô hại, không tự xoá; cân nhắc dọn khi user yêu cầu.
