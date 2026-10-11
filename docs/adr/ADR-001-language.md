# ADR-001: Ngôn ngữ + metadata store — Go + SQLite (WAL)

Bối cảnh: cần 1 binary duy nhất cài trên Linux/Windows/macOS, chạy daemon nền, bài toán IO/network đồng thời cao, RAM idle ≤150MiB, không phụ thuộc cloud.

Ứng viên đã xét (ma trận tech-selection): Go, Rust, Python, .NET.
- Rust: an toàn/hiệu năng cao nhất nhưng tốc độ phát triển + độ phức tạp async/filesystem watcher cao, rủi ro trễ.
- Python: không đóng gói binary tĩnh sạch, RAM/khởi động khó đạt tiêu chí daemon.
- .NET: mạnh trên Windows, service Linux/macOS và binary tĩnh (AOT) cồng kềnh hơn.

Lựa chọn: **Go 1.27** + **SQLite chế độ WAL** qua driver pure-Go `modernc.org/sqlite`.
Lý do: binary tĩnh CGO-free cross-compile cả 3 OS; goroutine + stdlib TLS 1.3/HTTP/gzip khớp sẵn bài toán; fsnotify là thư viện watcher chuẩn của hệ Go; SQLite WAL cho metadata bền vững + journal khôi phục sau crash, không cần server DB.
Đánh đổi: không có borrow-checker như Rust (bù bằng test + race detector); driver SQLite pure-Go chậm hơn bản CGO một chút (chấp nhận được ở quy mô metadata).
Xem lại khi: cần CDC/sync >100k file thường xuyên, hoặc hiệu năng SQLite thành nút thắt đo được.
