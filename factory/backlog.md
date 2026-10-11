# Backlog dọn dẹp (NIT từ review — không chặn merge)
- [ ] 001-NIT: validate — thêm reserved names CONIN$/CONOUT$; reject UTF-8 không hợp lệ (utf8.ValidString); ValidateSegment NFC-normalize trước check 255 byte; Error.Path giữ path gốc.
- [ ] 001-NIT: chunk — Bitmap.UnmarshalBinary chống tràn số khi header n cực lớn (Len âm).
- [ ] 001-NIT: vclock — marshal/unmarshal đối xứng với khoá rỗng; config.Load("") validate ở nhánh missing-file; ignore — pattern a//b nên báo lỗi thay vì lặng lẽ không khớp.
