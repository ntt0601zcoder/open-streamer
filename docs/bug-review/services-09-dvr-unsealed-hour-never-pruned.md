# Hour DVR dở dang sau khi process chết nằm lại catalog mãi và làm hỏng retention theo dung lượng

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/dvr/blob
- **Vị trí:** internal/dvr/blob/writer.go:272-278, internal/dvr/blob/recovery.go:18-35, internal/dvr/blob/recovery.go:121-125, internal/dvr/blob/service.go:86-99, internal/dvr/blob/service.go:307-310, internal/dvr/blob/retention.go:47-59, internal/dvr/blob/retention.go:70-93

## Hiện tượng
Đĩa DVR tăng dần vượt `retention_sec`: mỗi lần process thoát mà hour chưa seal để lại khoảng tới một giờ video mỗi profile không bao giờ bị xoá. Với `max_size_gb`, các hour mới hơn lại bị xoá sớm bất thường.

## Điều kiện kích hoạt
Process kết thúc khi đang ghi (crash, SIGKILL, hoặc SIGTERM thông thường vì shutdown không gọi `StopRecording`), rồi khởi động lại ở giờ UTC khác.

## Phân tích
`checkpointHour` ghi hour vào catalog với `Sealed=false` sau mỗi fragment. Lần start sau, `RepairStream` chỉ sửa file trên đĩa (đặt `HeaderFlagSealed`, xoá `.open`), không đụng catalog. `mergePriorCoverage` chép nguyên record:
```go
cat.Profiles[i].Hours = p.Hours
```
Retention bỏ qua record chưa seal:
```go
if !hr.Sealed {
    continue
}
```
Không còn writer nào cho hour đó nên `Sealed` giữ `false` vĩnh viễn; chỉ khi restart trong cùng giờ UTC thì `upsertHour` mới thay record.

`catalogTotalSize` vẫn cộng các hour này. Khi tổng hour mồ côi vượt ngân sách, `overSize` luôn đúng và vòng lặp xoá mọi hour đã seal ngay khi có.

## Vì sao đây là lỗi
Comment nói chỉ bảo vệ hour "in-progress và crash-dirty, not-yet-sealed", nhưng sau repair hour không còn dirty. Bản vá D-1 giữ lại coverage để retention thấy hour cũ, song chưa phủ trường hợp này.

## Hướng sửa
Sau `RepairStream` và merge, đánh dấu `Sealed=true` cho mọi hour không phải hour đang mở (có thể cập nhật `SizeBytes`, `WallToMs` từ `.ranges` đã sửa).

## Cách xác minh
Unit test: tạo catalog có hour `Sealed:false` ở giờ cũ, chạy `mergePriorCoverage` + `pruneOnce` với `RetentionSec` nhỏ, kiểm tra hour không bị xoá và file còn trên đĩa.
