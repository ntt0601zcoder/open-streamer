# Start xoá cờ exhausted vừa được đặt, stream chết vẫn báo Active

- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** internal/coordinator, internal/manager, internal/ingestor
- **Vị trí:** internal/coordinator/coordinator.go:362, internal/coordinator/coordinator.go:373-411, internal/manager/service.go:433-445, internal/manager/service.go:962-977, internal/ingestor/worker.go:264-266

## Hiện tượng
Stream mới start với nguồn hỏng ngay: `runtime.exhausted=true`, input Degraded, nhưng `status=active` và metric `streams_total{status="active"}`. Trạng thái sai giữ nguyên tới khi input phục hồi.

## Điều kiện kích hoạt
1. Stream một input, nguồn lỗi ngay. Ví dụ `copy://` tới upstream không tồn tại (`NewPacketReader` lỗi đồng bộ) hoặc connection refused.
2. Callback `go cb(streamID)` chạy xong trước dòng `clearDegradation` của `startLocked`. Phần `pub.Start`, `tc.Start`, `startDVR` (có I/O đĩa khi bật DVR) kéo dài khoảng cách này.

## Phân tích
`mgr.Register` kích hoạt ingest trước khi Start kết thúc:
```go
if err := c.mgr.Register(ctx, stream, ingestWriteID); err != nil {
...
c.startDVR(ctx, stream)
...
c.clearDegradation(stream.Code)
```
`handleExhausted` chỉ gọi callback ở cạnh lên:
```go
wasExhausted := state.exhausted
state.exhausted = true
...
if wasExhausted { return }
```
Nên sau khi cờ coordinator bị xoá, manager không báo lại, dù `state.exhausted` vẫn true.

## Vì sao đây là lỗi
Comment A-5 trong `Register` nói rõ muốn tránh "StreamStatus reported Active and the reconciler was blind", nhưng thứ tự trong `startLocked` lại xoá đúng tín hiệu đó. Mục đích của `clearDegradation` là xoá trạng thái của lần chạy trước, nên phải xảy ra trước khi có tín hiệu mới.

## Hướng sửa
Chuyển `clearDegradation` lên trước `mgr.Register`, và khi không có ingest cũng áp dụng tương tự.

## Cách xác minh
Test coordinator: `mgr` giả gọi callback exhausted bên trong `Register`, khẳng định `StreamStatus` là Degraded sau `Start`.
