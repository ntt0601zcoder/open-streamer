# Đổi thông số push destination cùng URL không có tác dụng trên stream đang chạy

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/coordinator, internal/publisher, internal/api
- **Vị trí:** internal/coordinator/diff.go:240-254, internal/coordinator/coordinator.go:625-629, internal/publisher/service.go:513-551, internal/publisher/push_rtmp.go:392-407, internal/publisher/push_rtmp.go:462-468

## Hiện tượng
`PUT` đổi `timeout_sec`, `retry_timeout_sec` hoặc `limit` của push destination (URL giữ nguyên, vẫn enabled): API trả 200, log báo `push_changed=true`, nhưng goroutine push vẫn dùng giá trị cũ tới khi stream restart.

## Điều kiện kích hoạt
Thay đổi một trong ba trường trên ở destination đang chạy.

## Phân tích
`pushSliceEqual` coi các trường đó là thay đổi, nhưng `updatePushDestinations` chỉ xử lý thêm, xoá và bật/tắt:
```go
nd, stillExists := newByURL[od.URL]
if !stillExists || (!nd.Enabled && od.Enabled) {
```
```go
if !existed || (!od.Enabled && nd.Enabled) {
```
`dest` được capture theo giá trị khi spawn, và `retryDelay`, `Limit`, `TimeoutSec` đọc từ đó.

## Vì sao đây là lỗi
Coordinator đã phát hiện đúng thay đổi và chuyển cho publisher, nhưng publisher không áp dụng. Nếu `limit` đã đạt và goroutine đã thoát, tăng `limit` cũng không khởi động lại push.

## Hướng sửa
Khi URL giữ nguyên và các trường khác đổi: gọi lại `spawnProtocolLocked` cho khoá `push:<url>` (hàm đã hỗ trợ respawn).

## Cách xác minh
Test publisher: `UpdateProtocols` đổi `RetryTimeoutSec` cùng URL, khẳng định goroutine cũ bị cancel và goroutine mới nhận giá trị mới (có thể quan sát qua hook test của `serveRTMPPush`).
