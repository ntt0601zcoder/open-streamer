# Đổi stream_key trên stream đang chạy không có hiệu lực

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/coordinator, internal/publisher, internal/ingestor/push, cmd/server
- **Vị trí:** internal/coordinator/diff.go:66-79, internal/coordinator/coordinator.go:625-629, internal/publisher/service.go:462-463, internal/publisher/playauth.go:41-48, cmd/server/main.go:433-447, internal/ingestor/push/rtmp_server.go:264-276, internal/api/handler/template.go:256-258

## Hiện tượng
Operator xoay vòng `stream_key` vì khoá bị lộ. API trả 200 và hiện khoá mới, nhưng RTMP server vẫn chấp nhận khoá cũ và từ chối khoá mới cho tới khi stream được restart.

## Điều kiện kích hoạt
1. Stream có input `publish://` và `stream_key: A` đang chạy.
2. `POST /streams/{code}` với `{"stream_key":"B"}`, hoặc đổi `stream_key` của template mà stream kế thừa.
3. Encoder kết nối lại bằng `?key=A`.

## Phân tích
`ComputeDiff` không có trường nào cho `StreamKey`, nên `updateLocked` không gọi publisher:

```go
if diff.ProtocolsChanged || diff.PushChanged || diff.PlaybackPolicyChanged {
    if err := c.pub.UpdateProtocols(ctx, old, new); err != nil {
```

`UpdateProtocols` là nơi duy nhất cập nhật bản sao trong bộ nhớ (`ss.streamKey = new.StreamKey`). Khi auth push, `wirePushAuth` ưu tiên bản sao này cho stream đang chạy:

```go
if sk, running := pub.PushStreamKey(code); running {
    return sk
}
```

Vì vậy store không được đọc tới khi stream còn chạy.

## Vì sao đây là lỗi
`PlaybackPolicy` có cờ `PlaybackPolicyChanged` riêng đúng vì lý do này, còn `StreamKey` thì không. Code chỉ đúng nếu tình cờ có thay đổi protocols, push hoặc policy trong cùng request. Không có test nào trong `internal/coordinator` nhắc tới `StreamKey`.

## Hướng sửa
Thêm `StreamKeyChanged` vào `StreamDiff` và đưa vào điều kiện gọi `UpdateProtocols`. Cân nhắc ngắt encoder đang giữ khoá cũ khi xoay vòng.

## Cách xác minh
Thêm test `ComputeDiff` chỉ đổi `StreamKey`. Thử thực tế: đổi khoá rồi push lại bằng khoá cũ, thấy vẫn được chấp nhận.
