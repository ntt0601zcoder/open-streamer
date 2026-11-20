# Đổi StreamKey trên stream đang chạy không có hiệu lực
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** coordinator, publisher, ingestor/push, cmd/server
- **Vị trí:** internal/coordinator/diff.go:66-79; internal/coordinator/coordinator.go:625-629; internal/publisher/service.go:294, 462-463; internal/publisher/playauth.go:41-48; cmd/server/main.go:433-436; internal/ingestor/push/rtmp_server.go:303-306

## Hiện tượng
Operator xoay vòng `stream_key` vì key cũ bị lộ. API trả 200 và `GET /streams/{code}` hiện key mới, nhưng encoder dùng key cũ vẫn được nhận, còn encoder dùng key mới bị từ chối với `unauthorized`. Trạng thái này giữ nguyên cho tới khi stream restart.

## Điều kiện kích hoạt
`PUT /streams/{code}` chỉ đổi `stream_key` trên stream `publish://` đang chạy. Cập nhật `stream_key` qua template cũng bị tương tự.

## Phân tích
Resolver push auth ưu tiên bản sao trong bộ nhớ của publisher:
```go
if sk, running := pub.PushStreamKey(code); running {
    return sk
}
```
`ss.streamKey` chỉ được gán ở `Publisher.Start` và `UpdateProtocols`. `ComputeDiff` không so sánh `StreamKey`, và coordinator chỉ gọi `UpdateProtocols` khi:
```go
if diff.ProtocolsChanged || diff.PushChanged || diff.PlaybackPolicyChanged {
```
Thay đổi chỉ ở `StreamKey` không làm bật cờ nào, nên `UpdateProtocols` không chạy.

## Vì sao đây là lỗi
`UpdateProtocols` đã có dòng `ss.streamKey = new.StreamKey`, cho thấy ý định là cập nhật nóng, nhưng diff không dẫn được tới dòng đó. `PlaybackPolicy` đã có cờ riêng cho đúng vấn đề này. Về bảo mật, key đã thu hồi vẫn dùng được trên stream đang chạy.

## Hướng sửa
Thêm so sánh `StreamKey` vào `ComputeDiff` (cờ `StreamKeyChanged` hoặc gộp vào nhánh refresh policy), rồi gọi `UpdateProtocols`. Cần cân nhắc có ngắt phiên encoder đang dùng key cũ hay không.

## Cách xác minh
Test: `ComputeDiff` với hai stream chỉ khác `StreamKey`, và test Update kiểm tra `pub.UpdateProtocols` được gọi. Trên server: đổi key rồi thử push bằng key cũ và key mới.
