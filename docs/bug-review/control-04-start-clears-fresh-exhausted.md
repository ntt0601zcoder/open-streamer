# Start xoá cờ exhausted vừa được đặt, stream có input hỏng vẫn báo Active
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** coordinator, manager
- **Vị trí:** internal/coordinator/coordinator.go:362, 373, 388, 403, 411; internal/manager/service.go:433-445, 962-977; internal/ingestor/worker.go:264-266

## Hiện tượng
Stream vừa start với nguồn không hợp lệ hoặc không kết nối được, nhưng `status` báo `active` trong khi `runtime.exhausted=true`. Dashboard và `open_streamer_streams_total{status="active"}` hiển thị xanh dù không có dữ liệu.

## Điều kiện kích hoạt
Một trong hai:
1. Lỗi đồng bộ: tất cả input đều không dựng được reader (mount `file://` không tồn tại, scheme sai). Stream một input là đủ.
2. Lỗi bất đồng bộ nhanh: `Open` thất bại ngay lần đầu (connection refused) trước khi `startLocked` chạy xong publisher, transcoder và DVR.

## Phân tích
`startLocked` gọi `mgr.Register` trước, rồi ở cuối mới xoá trạng thái degraded:
```go
if err := c.mgr.Register(ctx, stream, ingestWriteID); err != nil { ... }
...
c.startDVR(ctx, stream)
...
c.clearDegradation(stream.Code)
```
Trong `Register`, lỗi start đi qua `ReportInputError` → `handleExhausted` → `go cb(streamID)`, đặt `inputsExhausted=true`. Nếu callback chạy trước `clearDegradation`, cờ bị xoá. `handleExhausted` chỉ gọi callback ở lần chuyển trạng thái đầu:
```go
wasExhausted := state.exhausted
state.exhausted = true
...
if wasExhausted { return }
```
Vì vậy cờ không được đặt lại trong suốt thời gian nguồn còn chết.

## Vì sao đây là lỗi
Comment fix A-5 trong `Register` nói rõ stream một input sẽ "flips to Degraded". `clearDegradation` được viết để xoá trạng thái của lần chạy trước, nhưng lại đặt sau thời điểm lần chạy mới có thể báo lỗi. Thứ tự giữa goroutine callback và `clearDegradation` không được đảm bảo, nên hành vi không ổn định giữa các lần start.

## Hướng sửa
Gọi `clearDegradation` trước `mgr.Register`. Áp dụng tương tự cho `recordStartedAt` nếu cần, nhưng chỉ cờ degraded là bắt buộc.

## Cách xác minh
Test coordinator với mgr spy gọi callback exhausted bên trong `Register` và đợi nó hoàn tất. Sau `Start`, `StreamStatus` phải là `degraded`. Hiện tại trả `active`.
