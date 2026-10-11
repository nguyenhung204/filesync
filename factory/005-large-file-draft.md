# DRAFT CHƯA DUYỆT — Job 005: chính sách file lớn + kiểm chứng 20 GiB

Trạng thái: bản nháp soạn sẵn 2026-10-11, CHỜ user chốt. Chưa phải spec chính thức,
chưa spawn agent nào. Nguồn bối cảnh: `factory/jobs/004-engine-cli/large-file-20gb-brief.md`.

## Vì sao tách job riêng giờ là đường rẻ nhất
- Prompt builder round 2 của job 004 đã khoá (chỉ vá 3 finding round 1: race map daemon,
  trồng file vào `.filesync`, flood index) — nhét thêm chính sách file lớn vào round 2
  sẽ phình scope vòng sửa và phải review lại từ đầu.
- Tách 005: job 004 merge xong là spawn được ngay, spec dưới đây đã sẵn.

## Phạm vi (theo khuyến nghị A/A trong brief, user có thể đổi khi duyệt)
1. Chính sách version history: file có size > `VersionHistoryQuotaBytes` (mặc định 1 GiB)
   thì khi bị ghi đè bởi sync KHÔNG archive bản cũ vào version history; vẫn đồng bộ
   bình thường; ghi warning qua `internal/logx` và hiển thị trong `status`.
   File thường giữ nguyên hành vi. Trash khi xoá giữ nguyên cơ chế riêng.
2. Test đơn vị: file > quota bị sửa → không archive, có warning, bản mới áp dụng đúng;
   file ≤ quota vẫn archive như cũ (regression).
3. Test nặng tách riêng (không chạy trong `go test ./...` thường, theo tiền lệ e2e tag):
   file thưa 20 GiB, 2 node, kill bên nhận ở ~50% → nối lại truyền thêm <60%,
   hash đích khớp nguồn; ghi số thật vào build.md của job.

## Điều kiện máy dev (đã kiểm lại 2026-10-11 19:47 +07)
- Ổ trống 85 GB (df: 85G avail / 100G), RAM 7.9 GB — khớp số trong brief.
- Ngân sách đĩa cho test 2 node lần sync đầu (chưa có version): ~40–45 GB → khả thi.
  Test phải có precondition kiểm tra đĩa trống ≥50 GB, thiếu thì skip kèm lý do,
  và dọn file test sau khi chạy xong kể cả khi fail.
- Hash quét đầu 20 GiB ước 40–100s/máy → timeout test phải tính phần này.

## KHÔNG làm
- Không version-kiểu-delta (phương án C trong brief — để phase 2).
- Không đổi API job 001–004, không dependency mới, mọi file .go ≤500 dòng.

## Chưa chắc chắn
- Loại file 20 GB thật của user (video/archive/VM?) và tần suất sửa — nếu chỉ truyền
  1 lần không sửa, mục 1 gần như không ảnh hưởng thực tế.
- Tốc độ LAN thật của user chưa đo; số trong test là trên máy dev.
