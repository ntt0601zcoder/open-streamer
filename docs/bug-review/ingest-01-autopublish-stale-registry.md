# Slot push không bao giờ bị gỡ khi stream dừng, runtime stream không thể publish lại
- **Mức độ:** Nghiêm trọng
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor, ingestor/push, autopublish, coordinator, manager
- **Vị trí:** internal/ingestor/service.go:456-472; internal/ingestor/registry.go:53-58; internal/ingestor/push/rtmp_server.go:303-308, 459-460, 598-601; internal/autopublish/service.go:236-243, 329-350; internal/manager/service.go:490

## Hiện tượng
Một stream auto-publish đã bị reaper dọn (encoder rời đi hơn 30 s) không thể lên lại khi encoder đẩy lại đúng đường dẫn đó. Encoder nhận publish thành công và tiếp tục gửi dữ liệu, nhưng stream không xuất hiện trong runtime, người xem không có gì, và log chỉ có dòng Debug `buffer write failed`. Tình trạng kéo dài tới khi restart tiến trình.

Với stream cấu hình thường đã bị xoá hoặc dừng, encoder cũng được chấp nhận vào một buffer không tồn tại thay vì bị từ chối.

## Điều kiện kích hoạt
1. Template có prefix `live2` và input `publish://`. Encoder đẩy `rtmp://host/live2/foo`; `ResolveOrCreate` gọi `coord.Start`, slot `live2/foo` được `Register`.
2. Encoder ngắt. Sau khoảng 30 s reaper gọi `stopRuntimeStream` → `coord.Stop` → `mgr.Unregister` → `ingestor.Stop`.
3. Encoder đẩy lại cùng đường dẫn.

## Phân tích
`Service.Stop` chỉ huỷ pull worker và xoá callback, không gỡ registry:
```go
srv := s.rtmpSrv
delete(s.pendingPushCallbacks, streamID)
s.mu.Unlock()
if srv != nil { srv.ClearStreamCallbacks(streamID) }
```
`Registry.Unregister` không có caller nào ngoài test (`grep` toàn repo). Khi encoder đẩy lại, `acquireOrAutoPublish` thấy entry cũ và đi nhánh push trực tiếp, không bao giờ tới resolver:
```go
if _, streamID, _, lerr := s.registry.Lookup(key); lerr == nil {
    ...
    return s.registry.Acquire(key)
}
```
`Acquire` thành công, nhưng buffer đã bị `c.buf.Delete` (coordinator.go:526-527). `buf.SetSession` trả nil, `OnSession(nil)` là no-op, session vẫn được chấp nhận. Mỗi `p.buf.Write` sau đó trả lỗi `stream not found` và chỉ log Debug.

## Vì sao đây là lỗi
Comment trong `observeLiveness` của autopublish giả định ngược với code: "the ingestor's push registry slot is already gone". Reconciler chỉ duyệt stream đã lưu nên không cứu được runtime stream. Fast path `ResolveOrCreate` không chạy vì resolver không được gọi. Callback cũng đã bị xoá, nên manager không nhận được gì.

## Hướng sửa
Lưu key đã đăng ký theo stream và gọi `registry.Unregister(key)` trong `Service.Stop`. Cân nhắc đóng luôn publisher đang kết nối (xem ingest-04). Trong `OnNewRtmpPubSession`, từ chối session khi `SetSession` trả nil.

## Cách xác minh
Test tích hợp với `autopublish` + `push.RTMPServer` giả: Start, Stop, rồi gọi `acquireOrAutoPublish(key)`; kỳ vọng resolver được gọi lại. Thủ công trên môi trường thử: đẩy RTMP vào một prefix, ngắt 40 s, đẩy lại, kiểm tra `GET /streams` không có runtime stream và `/{code}/index.m3u8` không tồn tại.
