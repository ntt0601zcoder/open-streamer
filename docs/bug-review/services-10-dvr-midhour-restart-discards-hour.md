# Restart stream giữa giờ xoá toàn bộ phần đã ghi của giờ UTC đó

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/dvr/blob, internal/coordinator
- **Vị trí:** internal/dvr/blob/writer.go:396-403, internal/dvr/blob/writer.go:290-295, internal/dvr/blob/service.go:251-259, internal/dvr/blob/recovery.go:10-17, internal/coordinator/coordinator.go:705, internal/coordinator/coordinator.go:727-730

## Hiện tượng
Sau restart stream, đổi ladder, đổi cấu hình DVR hoặc restart server, phần video từ đầu giờ UTC tới lúc đó biến mất, tối đa gần 60 phút cho mỗi profile. `recording_status.json` vẫn báo `dvr_range` bao gồm khoảng đã mất, và player gặp 404 hoặc playlist rỗng.

## Điều kiện kích hoạt
Stream có DVR đang ghi, lúc 14:50 UTC gọi `POST /streams/{code}/restart` hoặc đổi transcoder. Khi lane mở lại giờ 14, các file của giờ này bị xoá, kể cả khi đã seal sạch.

## Phân tích
```go
if hourFilesExist(stem) {
    slog.Warn("blob: resuming into existing hour; discarding partial hour", ...)
    removeHourFiles(stem)
}
```
Kích hoạt từ mọi `StartRecording`: `reloadTranscoderFull` gọi `reloadDVR`, `diff.DVRChanged` cũng gọi `reloadDVR`.

Catalog không phản ánh việc xoá: `extendAvailable` thấy `WallFromMs` mới lớn hơn `ToMs` cũ nên thêm window mới và giữ window cũ.

## Vì sao đây là lỗi
Comment ghi "by design", nhưng cái giá là mất dữ liệu đã ghi bền vững, và `RepairStream` vừa chạy ngay trước đó để cứu đúng các file này, với mô tả "the writer opens a fresh hour with a discontinuity". Một thay đổi cấu hình không liên quan tới DVR không nên xoá bản ghi.

## Hướng sửa
Mở lại hour đã repair ở chế độ append (`openBlobFile` đã có sẵn nhưng chưa dùng): nạp `.ranges`, tiếp tục offset và seq, đánh dấu discontinuity cho fragment đầu. Nếu giữ thiết kế xoá thì tối thiểu phải tính lại `Available`.

## Cách xác minh
Unit test: dùng `newProfileWriter` ghi fragment trong một giờ, `Close`, tạo writer mới cùng giờ, ghi tiếp, kiểm tra fragment cũ còn trong `.ranges` (hiện bị mất).
