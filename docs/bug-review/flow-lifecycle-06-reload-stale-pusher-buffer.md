# Reload transcoder với encoder đang push làm output đen hoặc failover giả

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/coordinator, internal/manager, internal/ingestor, internal/ingestor/push
- **Vị trí:** internal/coordinator/coordinator.go:683-689, internal/manager/service.go:1231-1264, internal/ingestor/service.go:619-659, internal/ingestor/push/rtmp_server.go:445-452, internal/ingestor/push/rtmp_server.go:598

## Hiện tượng
Bật hoặc tắt transcoder trên stream `publish://` đang live: encoder vẫn gửi, status Active, nhưng viewer không có dữ liệu cho tới khi encoder kết nối lại. Với stream nhiều input, chỉ cần đổi ladder cũng gây failover giả sang backup.

## Điều kiện kích hoạt
1. Encoder đang push vào stream.
2. `PUT` chuyển passthrough sang có transcoder (hoặc ngược lại), đi qua `reloadTranscoderFull`.
3. Riêng nhánh failover giả: stream có từ hai input trở lên, bất kỳ thay đổi transcoder nào đi qua reload.

## Phân tích
`pubState` chốt đích ghi lúc kết nối:
```go
bufferWriteID: bufWriteID,
```
`UpdateBufferWriteID` chỉ gọi lại `ingestor.Start`; với push, hàm đó chỉ cập nhật registry cho phiên tương lai. Phiên hiện tại vẫn ghi vào buffer cũ:
- nil sang transcoder: ghi vào `<code>`, transcoder đọc `$raw$<code>` rỗng.
- transcoder sang nil: ghi vào `$raw$<code>` đã bị xoá, `Write` lỗi chỉ ở Debug, publisher đọc `<code>` rỗng.

`RecordPacket` vẫn chạy nên manager coi input là khoẻ và không kích hoạt recovery.

`startPushRegistration` luôn báo `ErrNoPusherConnected`; với nhiều input, input đang nhận packet bị đánh Degraded và chuyển sang backup, sau đó failback.

## Vì sao đây là lỗi
Comment của `UpdateBufferWriteID` hứa "The active ingestor is restarted to write to the new buffer", điều này chỉ đúng với pull worker.

## Hướng sửa
Thêm thao tác trên `RTMPServer` để đổi `bufferWriteID` của pub session đang chạy (đồng bộ an toàn với `OnReadRtmpAvMsg`) rồi mint session mới, hoặc Dispose session để encoder kết nối lại. Bỏ qua `ErrNoPusherConnected` khi slot đang có pusher.

## Cách xác minh
Trên môi trường thử: push RTMP, `PUT` thêm transcoder, đo buffer `$raw$<code>` không có subscriber nhận dữ liệu và HLS rendition không có segment mới. Với hai input, xem `switches[]` có sự kiện `error` ngay sau reload.
