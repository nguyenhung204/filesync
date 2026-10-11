# Backlog dọn dẹp (NIT từ review — không chặn merge)

## GATE từ approver job 001 (decision.md) — bắt buộc trước job 003 build
- [ ] validate: reject UTF-8 không hợp lệ + thêm reserved names CONIN$/CONOUT$ (cập nhật spec job 001 trước khi thêm).
- [ ] chunk: Bitmap.UnmarshalBinary chống tràn số (n cực lớn → Len âm) — bắt buộc trước khi bitmap nạp từ nguồn kém tin cậy hơn journal local; tầng sync không kết luận "đủ chunk" chỉ dựa vào Missing() cho tới khi vá.

## NIT thường (job dọn dẹp 002c xử lý luôn nếu tiện)
- [ ] validate: ValidateSegment NFC-normalize trước check 255 byte; Error.Path giữ path gốc.
- [ ] vclock: marshal/unmarshal đối xứng với khoá rỗng; config.Load("") validate ở nhánh missing-file; ignore: pattern a//b báo lỗi thay vì lặng lẽ.
