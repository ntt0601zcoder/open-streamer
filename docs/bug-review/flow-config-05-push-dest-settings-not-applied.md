# Đổi timeout, retry, limit của push destination không có tác dụng

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/coordinator, internal/publisher
- **Vị trí:** internal/coordinator/diff.go:240-254, internal/coordinator/coordinator.go:625-629, internal/publisher/service.go:513-551, internal/publisher/push_rtmp.go:394-417

## Hiện tượng
Operator đổi `timeout_sec`, `retry_timeout_sec` hoặc `limit` của một đích push. API trả 200, log có `push_changed=true`, nhưng worker vẫn chạy với giá trị cũ. Đích đã dừng vì hết `limit` cũng không chạy lại khi tăng `limit`.

## Điều kiện kích hoạt
1. Stream đang push tới `rtmp://x` với `limit: 3`, worker đã dừng ở trạng thái `failed`.
2. `POST /streams/{code}` đổi `limit: 0`, cùng URL và vẫn `enabled: true`.

## Phân tích
`pushSliceEqual` nhận ra thay đổi, nên `UpdateProtocols` được gọi. Nhưng `updatePushDestinations` chỉ hành động khi URL mới hoặc khi `Enabled` đổi trạng thái:

```go
if !existed || (!od.Enabled && nd.Enabled) {
```

Goroutine `serveRTMPPush` đã giữ bản sao `dest` từ lúc spawn, `retryDelay` và `dest.Limit` chỉ đọc từ bản sao đó.

## Vì sao đây là lỗi
Diff engine đánh dấu đã đổi nhưng lớp áp dụng bỏ qua, nên log báo thay đổi trong khi không có gì xảy ra. Cách lách duy nhất là tắt rồi bật đích, tức hai lần lưu.

## Hướng sửa
Trong `updatePushDestinations`, khi URL còn tồn tại, bật và có `TimeoutSec`, `RetryTimeoutSec` hoặc `Limit` khác thì respawn. `spawnProtocolLocked` đã hủy worker cùng key.

## Cách xác minh
Test publisher với hai cấu hình chỉ khác `Limit`: goroutine `push:<url>` phải được spawn lại. Thử thực tế: đích không tới được với `limit: 1`, sau đó đổi `limit: 0` và thấy không có lần kết nối mới.
