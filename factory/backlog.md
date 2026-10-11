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
