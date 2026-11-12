# Đổi stream_key khi stream đang chạy không có hiệu lực

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api, internal/coordinator, internal/publisher, internal/ingestor/push, cmd/server
- **Vị trí:** internal/coordinator/diff.go:66-79, internal/coordinator/coordinator.go:625-629, internal/publisher/service.go:461-463, internal/publisher/playauth.go:41-48, cmd/server/main.go:433-436, internal/ingestor/push/rtmp_server.go:264-276

## Hiện tượng
Operator đổi khoá push vì lộ khoá: `PUT` trả 200 và store có khoá mới, nhưng server vẫn chấp nhận khoá cũ và từ chối khoá mới tới khi stream restart.

## Điều kiện kích hoạt
`PUT /streams/<code>` hoặc sửa template chỉ thay `stream_key`, stream đang chạy.

## Phân tích
`ComputeDiff` không so sánh `StreamKey`, nên `updateLocked` không gọi publisher. Bản sao trong RAM chỉ được làm mới ở `UpdateProtocols`, mà hàm đó chỉ chạy khi:
```go
if diff.ProtocolsChanged || diff.PushChanged || diff.PlaybackPolicyChanged {
```
Resolver push đọc bản RAM trước:
```go
if sk, running := pub.PushStreamKey(code); running {
    return sk
}
```
Nên store không được đọc khi stream đang chạy.

## Vì sao đây là lỗi
`PlaybackPolicy` có đúng dạng bản sao này và đã được thêm vào diff với lý do tương tự; `StreamKey` bị bỏ sót. CLAUDE.md cảnh báo trường không nằm trong `ComputeDiff` sẽ không áp dụng cho stream đang chạy.

## Hướng sửa
Thêm `StreamKeyChanged` vào `StreamDiff` và đi cùng đường `UpdateProtocols`, hoặc để resolver đọc store. Cân nhắc ngắt pusher đang dùng khoá cũ.

## Cách xác minh
Test coordinator với publisher thật hoặc spy: `Update` chỉ đổi `StreamKey`, khẳng định `PushStreamKey` trả khoá mới. Test diff tương ứng.
