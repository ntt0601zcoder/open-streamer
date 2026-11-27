# Slot push không bao giờ được gỡ, stream auto-publish đã reap không thể tạo lại

- **Mức độ:** Nghiêm trọng
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/ingestor, internal/ingestor/push, internal/autopublish, internal/coordinator, internal/manager, internal/api
- **Vị trí:** internal/ingestor/service.go:455-472, internal/ingestor/service.go:625, internal/ingestor/registry.go:53-58, internal/ingestor/push/rtmp_server.go:303-308, internal/ingestor/push/rtmp_server.go:459-460, internal/ingestor/push/rtmp_server.go:598-601, internal/autopublish/service.go:233-242, internal/autopublish/service.go:424-429

## Hiện tượng
Encoder push vào đường dẫn auto-publish, ngắt hơn 30 giây rồi push lại: encoder báo kết nối thành công nhưng không có stream, không có output, `GET /streams` không thấy gì, kéo dài tới khi restart server. Tương tự, stream cấu hình đã xoá vẫn nhận push vào hư không. Encoder đang kết nối cũng không bị ngắt khi stream bị stop.

## Điều kiện kích hoạt
1. Stream có input `publish://` chạy, `startPushRegistration` gọi `registry.Register`.
2. Stream dừng vì idle reaper, `DELETE`, `restart` runtime, v.v.
3. Encoder kết nối lại cùng path.

## Phân tích
`Ingestor.Stop` có doc "cancels the pull worker or unregisters the push key" nhưng chỉ xoá callback:
```go
delete(s.pendingPushCallbacks, streamID)
...
srv.ClearStreamCallbacks(streamID)
```
Không có chỗ nào ngoài test gọi `Registry.Unregister`. Khi encoder quay lại, `acquireOrAutoPublish` thấy `Lookup` thành công nên trả về `Acquire` luôn và không bao giờ gọi resolver. Buffer đã bị xoá nên `SetSession` trả nil (`OnSession(nil)` là no-op) và mỗi `buf.Write` lỗi chỉ được log ở mức Debug. Callback rỗng nên manager không biết gì.

## Vì sao đây là lỗi
`observeLiveness` ghi rõ giả định sai: "the ingestor's push registry slot is already gone". `StopRuntime` cũng giả định "encoder's current push session breaks on the next write", trong khi `OnReadRtmpAvMsg` nuốt lỗi Write và không có đoạn nào Dispose pub session. Test autopublish dùng coordinator giả nên không chạm registry thật.

## Hướng sửa
`Ingestor.Stop` phải `registry.Unregister` khoá tương ứng (cần lưu key theo stream) và Dispose pub session đang chạy. Nên có API trên `RTMPServer` để đóng session theo streamID.

## Cách xác minh
Trên môi trường thử: tạo template có prefix và input `publish://`, push, ngắt 40 giây, push lại. Kỳ vọng hiện tại: không có log `autopublish: runtime stream materialised` lần hai, log `publisher accepted` vẫn xuất hiện. Có thể viết integration test với `Registry` thật và kiểm `Lookup` sau `Stop`.
