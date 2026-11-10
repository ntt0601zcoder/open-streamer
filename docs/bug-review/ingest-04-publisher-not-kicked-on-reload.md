# Encoder đang kết nối giữ bufferWriteID cũ khi stream reload hoặc restart
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/push, ingestor, coordinator, manager
- **Vị trí:** internal/ingestor/push/rtmp_server.go:444-453, 581-606; internal/ingestor/registry.go:38-51; internal/ingestor/service.go:456-472, 619-660; internal/coordinator/coordinator.go:640-690; internal/manager/service.go:1231-1264

## Hiện tượng
Bật hoặc tắt transcoder qua `PUT /streams/{code}` khi encoder RTMP đang đẩy làm người xem mất hình, trong khi UI vẫn báo input Active. Stream chỉ phục hồi khi encoder tự ngắt kết nối. Đồng thời, encoder thứ hai có thể chiếm cùng stream key dù encoder đầu còn kết nối.

## Điều kiện kích hoạt
1. Stream `publish://` không có transcoder, encoder đang đẩy và `pubState.bufferWriteID = <code>`.
2. Operator thêm transcoder; `reloadTranscoderFull` đổi ingest sang `$raw$<code>`.
3. `UpdateBufferWriteID` gọi `ingestor.Start`, tức `startPushRegistration`.

Tương tự với chiều bỏ transcoder, hoặc Stop rồi Start khi encoder chưa ngắt.

## Phân tích
ID buffer chỉ được lấy lúc publish:
```go
state := &pubState{ key: key, bufferWriteID: bufWriteID, ... cb: cb, ... }
```
`startPushRegistration` chỉ gọi `registry.Register`, không cập nhật `s.pubs`. Encoder tiếp tục ghi vào `<code>`, trong khi transcoder đọc `$raw$<code>` rỗng và publisher đọc rendition. Khi bỏ transcoder, `oldIngestID` bị `Delete` và `Write` trả lỗi, nhưng chỉ log Debug.

Liveness vẫn báo tốt vì callback chạy trước khi ghi:
```go
if p.cb.OnPacket != nil { p.cb.OnPacket() }
...
if err := p.buf.Write(p.bufferWriteID, buffer.Packet{AV: q}); err != nil {
    slog.Debug("rtmp server: buffer write failed", ...)
}
```
`Register` tạo entry mới với `active=false` (comment: "clears any stale active-pusher mark"), nên `Acquire` của encoder thứ hai thành công. Khi session cũ kết thúc, `Release(state.key)` lại xoá dấu active của session mới, và `OnDisconnect` cũ đánh Degraded input đang chạy tốt.

## Vì sao đây là lỗi
Comment của `UpdateBufferWriteID` hứa "The active ingestor is restarted to write to the new buffer"; điều này chỉ đúng với pull. Read timeout 10 s của lal không giúp vì encoder vẫn đang gửi dữ liệu. `ARCHITECTURE.md` yêu cầu mỗi slot chỉ có một pusher.

## Hướng sửa
Khi Register/Stop, tìm các `pubState` theo key và dispose session để encoder kết nối lại, hoặc cập nhật target nguyên tử cho `pubState`. `Release` cần định danh theo session (token) để session cũ không xoá trạng thái của session mới. Chỉ gọi `OnPacket` khi `Write` thành công.

## Cách xác minh
Test: `Register` + `Acquire` + `Register` lại, kiểm tra `Acquire` lần hai hiện thành công. Test `RTMPServer` với buffer giả: đổi bufferWriteID và kiểm tra ghi đi đâu. Thủ công: đẩy RTMP, bật transcoder qua API, xem output trống trong khi `last_packet_at` vẫn tăng.
