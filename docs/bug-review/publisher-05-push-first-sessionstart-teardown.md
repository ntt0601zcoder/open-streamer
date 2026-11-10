# RTMP push tự ngắt phiên vừa kết nối khi gặp SessionStart đầu tiên
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (RTMP push), buffer, ingestor
- **Vị trí:** internal/publisher/push_rtmp.go:286-292, 306-309, 423-428; internal/buffer/service.go (ringBuffer.write); internal/coordinator/coordinator.go:373

## Hiện tượng
Mỗi lần stream khởi động lạnh, đích RTMP thấy một kết nối publish được mở rồi đóng ngay, sau đó một kết nối thứ hai. Webhook nhận `push.active` hai lần. Một số đích từ chối publish lại quá nhanh với cùng stream key.

## Điều kiện kích hoạt
Goroutine push gọi `Subscribe` trước khi có packet đầu tiên của session. Điều này gần như luôn đúng với pull qua mạng, RTMP push ingest (chờ encoder) và stream transcode (subprocess khởi động chậm).

## Phân tích
`SetSession` đặt cờ một lần, packet đầu sau đó mang `SessionStart=true`. `feedLoop` coi mọi cờ này là gián đoạn khi `connErr` rỗng, mà `connErr` rỗng chính là trạng thái bình thường:
```go
if pkt.SessionStart && p.connErr.Load() == nil {
    p.gotDiscontinuity.Store(true)
    return
}
```
Comment nói SessionStart đầu tiên "is consumed by the initial setup path", nhưng không có đường xử lý nào như vậy.

## Vì sao đây là lỗi
Phiên hiện tại chưa gửi media nào nên không có gì cần khởi động lại. Packet bị bỏ thường là IDR đầu tiên, nên phiên thứ hai bắt đầu giữa GOP (xem publisher-06). Vòng `serveRTMPPush` reset `attempts` và reconnect không delay, che mất hiện tượng này.

## Hướng sửa
Chỉ coi là gián đoạn nếu phiên đã forward ít nhất một packet. Packet SessionStart đầu tiên thì xử lý bình thường.

## Cách xác minh
Test với `sub` có packet `SessionStart=true` đầu tiên phải không trả `errDiscontinuity`. Thực tế: push tới server cục bộ, đếm số kết nối khi start stream.
