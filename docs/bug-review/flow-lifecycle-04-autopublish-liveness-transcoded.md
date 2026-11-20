# Stream auto-publish có transcoder bị reaper giết sau 30 giây dù encoder vẫn đẩy

- **Mức độ:** Nghiêm trọng
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/autopublish, internal/coordinator, internal/transcoder, internal/buffer
- **Vị trí:** internal/autopublish/service.go:248-268, internal/autopublish/service.go:312-325, internal/coordinator/coordinator.go:331-337, internal/transcoder/supervisor.go:438-462, internal/ingestor/push/rtmp_server.go:598

## Hiện tượng
Template có prefix, input `publish://` và transcoder đang encode: runtime stream chạy khoảng 30-35 giây rồi bị dừng với lý do `idle`, dù encoder vẫn gửi đều. Kết hợp với flow-lifecycle-03, stream không tự lên lại.

## Điều kiện kích hoạt
Một lần push vào path khớp template có `transcoder` với `video.copy=false` hoặc `audio.copy=false`.

## Phân tích
Observer chỉ đăng ký buffer chính:
```go
sub, err := s.buf.Subscribe(entry.code)
```
Nhưng với transcoder, `startLocked` chuyển ingest sang buffer khác:
```go
if shouldRunTranscoder(stream) {
    ingestWriteID = buffer.RawIngestBufferID(stream.Code)
```
Transcoder ghi vào `$r$<code>$track_N`. Rà toàn bộ `buf.Write` cho thấy không có ai ghi vào buffer `<code>` trong topology này, nên `lastPacketAt` giữ nguyên giá trị lúc tạo. `reapOnce` thấy quá `IdleTimeout` và gọi `stopRuntimeStream(..., "idle")`.

## Vì sao đây là lỗi
Buffer `<code>` vẫn được tạo nên `Subscribe` thành công và đường `subscribe_failed` không kích hoạt. Tài liệu nói reaper dựa trên "last packet reaches the buffer hub", nhưng observer nhìn sai buffer. Test autopublish không có trường hợp template có transcoder.

## Hướng sửa
Theo dõi đúng buffer ghi ingest (`$raw$<code>` khi có transcoder), ví dụ qua helper dùng chung với coordinator, hoặc lấy liveness từ manager (`LastPacketAt`).

## Cách xác minh
Unit test với buffer thật: tạo `<code>` và `$raw$<code>`, ghi đều vào raw và khẳng định entry không bị reap. Trên môi trường thử: push vào template có transcoder và xem log `runtime stream expired ... reason=idle` sau khoảng 30 giây.
