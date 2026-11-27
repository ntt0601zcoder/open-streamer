# Cờ transcoder unhealthy kẹt sau một lần crash nhanh dù subprocess đã ổn định

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder, internal/coordinator, internal/api
- **Vị trí:** internal/transcoder/supervisor.go:106-143, internal/transcoder/supervisor.go:271-274, internal/transcoder/service.go:254-264, internal/coordinator/coordinator.go:829-855, docs/ARCHITECTURE.md:446-450

## Hiện tượng
Transcoder crash một lần trong 30 giây đầu rồi respawn và chạy tốt nhiều giờ, nhưng stream vẫn `degraded`, `runtime.transcoder.status=unhealthy`. Ngược lại, khi crash-loop thật, status và sự kiện dao động theo từng lần crash.

## Điều kiện kích hoạt
Subprocess thoát với lỗi khi đã chạy dưới `fastCrashThreshold` (30 giây), ví dụ thiếu session GPU tạm thời, rồi lần respawn sau chạy ổn định.

## Phân tích
Chỉ có một nơi gọi trạng thái healthy, và nó nằm ở cuối `runOnce`, tức lúc subprocess đã thoát:
```go
sv.svc.fireHealthyIfTransitioned(sv.streamID)
```
Trong `Run`, ngay sau đó:
```go
if runtime < fastCrashThreshold {
    sv.svc.fireUnhealthyIfTransitioned(sv.streamID, err.Error())
```
Một subprocess chạy mãi không bao giờ đi tới dòng healthy. Khi crash-loop, mỗi vòng lại tạo cặp healthy rồi unhealthy, sinh log `transcoder recovered` và `EventTranscoderError` mỗi lần.

## Vì sao đây là lỗi
Tài liệu ghi: vượt 3 fast crash mới báo `onUnhealthy`, và "a sustained run (≥30s) fires `onHealthy`". Code không có cả ngưỡng 3 lần lẫn đồng hồ sustain. Test `health_test.go` chỉ kiểm tra logic cạnh, không kiểm tra thời điểm gọi.

## Hướng sửa
Bỏ lời gọi ở cuối `runOnce` cho trường hợp crash. Thêm timer trong `runOnce` (hoặc theo dõi output đầu tiên) để gọi healthy sau khi chạy đủ `fastCrashThreshold`. Đếm số crash nhanh liên tiếp trước khi báo unhealthy.

## Cách xác minh
Test supervisor với binary giả: lần đầu thoát ngay, lần sau chạy 31 giây, khẳng định callback healthy được gọi khi process vẫn sống và `StreamStatus` về Active.
