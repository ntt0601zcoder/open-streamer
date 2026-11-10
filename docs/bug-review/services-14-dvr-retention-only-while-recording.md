# Retention chỉ chạy khi đang ghi, archive của stream đã dừng tồn tại vô thời hạn

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/dvr/blob, internal/api/handler, internal/autopublish
- **Vị trí:** internal/dvr/blob/service.go:130-131, internal/dvr/blob/retention.go:18-33, internal/api/handler/stream.go:706-734

## Hiện tượng
Stream đặt `retention_sec=3600` rồi bị disable, xoá, hoặc là runtime stream đã kết thúc: dữ liệu giữ nguyên trên đĩa và vẫn phát được sau nhiều ngày. Với auto-publish, mỗi code mới để lại một thư mục archive không ai dọn.

## Điều kiện kích hoạt
Stream có DVR và retention, sau đó pipeline dừng lâu hơn cửa sổ retention. Stream flapping dưới 60 giây cũng không bao giờ tới lượt prune đầu tiên.

## Phân tích
Retention là goroutine theo recording, bị huỷ cùng `StopRecording`, và ticker đầu tiên chỉ chạy sau 60 giây:
```go
go s.runRetention(rctx, rec, cfg)
...
t := time.NewTicker(retentionInterval)
```
Không có sweep nào ở cấp service, lúc boot, hay khi xoá stream. `StreamHandler.Delete` không xoá Recording row hay file.

## Vì sao đây là lỗi
Cấu hình `retention_sec` được hiểu là giới hạn tuổi dữ liệu, kể cả khi cần tuân thủ quy định lưu trữ. Bản vá D-1 cho thấy ý định là tránh đĩa tăng không giới hạn, và catalog đã chép `Retention` vào để "central pass is self-contained", nhưng central pass chưa tồn tại.

## Hướng sửa
Thêm sweep nền ở cấp `blob.Service`, duyệt Recording rows và catalog không active, áp `cat.Retention` (lưu ý hiện `MaxSizeGB` bị ép `int`). Định nghĩa rõ việc xử lý archive khi xoá stream.

## Cách xác minh
Test: `StartRecording` với file giả đã cũ, `StopRecording`, tiến thời gian, kiểm tra không có gì bị xoá. Hoặc đọc `recording_status.json` của stream đã dừng lâu hơn retention.
