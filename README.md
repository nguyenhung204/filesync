# FileSync

Công cụ đồng bộ file 2 chiều giữa nhiều máy trong LAN — giống Syncthing thu gọn.
1 binary duy nhất, chạy daemon nền, **không qua cloud trung gian**, không telemetry.

Trạng thái: đang xây dựng (phase 1). Tài liệu đầy đủ (kiến trúc, cài service 3 OS,
pair, xử lý conflict, giới hạn đã biết) sẽ hoàn thiện cùng bản phát hành đầu tiên —
xem `docs/adr/` cho các quyết định kiến trúc và `docs/domain.md` cho nghiệp vụ.

## Quyết định đã chốt (2026-10-11)
- Đích: Linux + Windows + macOS. Mạng: LAN + IP tĩnh + mDNS (relay: phase 2).
- Conflict: giữ cả 2 bản, không bao giờ ghi đè mất dữ liệu (ADR-003).
- Go + SQLite (WAL) (ADR-001). Chunking fixed 1MiB (ADR-002).
