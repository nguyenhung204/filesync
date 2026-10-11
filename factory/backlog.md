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
