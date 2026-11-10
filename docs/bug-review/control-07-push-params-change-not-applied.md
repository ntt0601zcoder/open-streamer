# Đổi timeout hoặc limit của push destination đang chạy không có hiệu lực
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** coordinator, publisher
- **Vị trí:** internal/coordinator/diff.go:240-254; internal/coordinator/coordinator.go:625-629; internal/publisher/service.go:513-551

## Hiện tượng
Operator đổi `timeout_sec`, `retry_timeout_sec` hoặc `limit` của một push destination đang bật. API trả 200 nhưng worker push vẫn chạy với giá trị cũ, kể cả khi đích liên tục lỗi và cần giới hạn retry mới.

## Điều kiện kích hoạt
`PUT /streams/{code}` giữ nguyên `url` và `enabled` của destination, chỉ đổi một trong ba trường trên.

## Phân tích
`pushSliceEqual` coi đây là thay đổi, nên `diff.PushChanged=true` và `UpdateProtocols` được gọi. Nhưng `updatePushDestinations` chỉ xét URL và `Enabled`:
```go
if !stillExists || (!nd.Enabled && od.Enabled) {
    s.stopProtocol(code, "push:"+od.URL)
}
...
if !existed || (!od.Enabled && nd.Enabled) {
```
Goroutine đang chạy giữ bản sao `d` đã capture từ lúc spawn, nên không thấy giá trị mới.

## Vì sao đây là lỗi
Diff và phía áp dụng không khớp nhau: diff báo có thay đổi cho từng trường này, còn publisher bỏ qua. Không có đường nào khác cập nhật worker.

## Hướng sửa
Trong `updatePushDestinations`, khi URL còn tồn tại và cả hai bên đều enabled mà `TimeoutSec`, `RetryTimeoutSec` hoặc `Limit` khác nhau, gọi lại `spawnProtocolLocked` cho key đó. Hàm này đã tự cancel worker cũ.

## Cách xác minh
Test publisher: gọi `UpdateProtocols` với destination chỉ khác `Limit`, kiểm tra worker cũ bị cancel và worker mới nhận `Limit` mới.
