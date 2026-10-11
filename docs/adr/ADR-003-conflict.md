# ADR-003: Conflict — version vector, giữ cả 2 bản

Bối cảnh: nhiều máy sửa đồng thời khi offline, đồng hồ hệ thống lệch nhau, yêu cầu cứng "không mất dữ liệu trong mọi tình huống" (user chốt 2026-10-11).

Ứng viên: last-write-wins theo mtime (loại: lệch giờ = mất dữ liệu lặng lẽ), last-write-wins theo version vector (loại: vẫn mất bản thua ở thư mục chính), version vector + giữ cả 2.

Lựa chọn: **version vector theo thiết bị** là thẩm quyền thứ tự duy nhất (không dùng đồng hồ để quyết thắng thua):
- Vector của bản đến **dominate** bản hiện tại → áp dụng thẳng.
- **Concurrent** (không vector nào dominate) → conflict: giữ cả 2, bản đến đổi tên `tên (conflict <device> <timestamp>)`, user xử lý thủ công qua CLI `conflicts`.
- Xóa vs sửa concurrent → **bản sửa thắng**, huỷ tombstone (xóa là hành động dễ làm lại, sửa là dữ liệu không tái tạo được).
- Tombstone có TTL 30 ngày; quá TTL thì quên hẳn, tránh resurrect file đã xóa lâu khi máy offline quay lại.

Đánh đổi: thư mục có thể xuất hiện file conflict cần user dọn tay — đó là giá của "không bao giờ mất dữ liệu", đúng chính sách user đã chốt.
