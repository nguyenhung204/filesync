# ADR-002: Chunking — fixed-size 1MiB

Bối cảnh: cần delta sync + resume theo bản đồ chunk đã nhận + verify từng chunk.

Ứng viên: fixed-size 1MiB vs content-defined chunking (CDC — FastCDC/buzhash).
- CDC thắng khi chèn/xoá ở giữa file (chỉ lệch 1 vùng); thua ở độ phức tạp, biên chunk biến thiên khó test, rủi ro bug cao hơn.
- Fixed 1MiB: biên xác định tuyệt đối → resume map = bitmap theo index, cực đơn giản và test chắc; hash SHA-256 từng chunk; file sửa kiểu append/sửa tại chỗ (đa số file thực tế) vẫn chỉ truyền chunk khác. Nhược: chèn 1 byte ở đầu file lớn → mọi chunk lệch (chấp nhận phase 1, ghi vào giới hạn đã biết).

Lựa chọn: **fixed 1MiB**, chunk cuối ngắn hơn; giao thức mang `chunk_size` trong index để phase 2 có thể thêm CDC mà không phá tương thích (tăng protocol version nếu đổi ngữ nghĩa).
Xem lại khi: có số liệu thực tế nhiều file lớn bị sửa kiểu chèn-giữa.
