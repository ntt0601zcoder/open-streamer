# Restart listener RTMP làm mất play handler, auto-publish, xác thực push và callback

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/runtime, internal/ingestor, internal/ingestor/push, internal/manager, internal/publisher, internal/autopublish
- **Vị trí:** internal/ingestor/service.go:228-268, internal/ingestor/service.go:175-222, internal/runtime/manager.go:284-291, internal/runtime/manager.go:200-214, internal/ingestor/push/rtmp_server.go:264-276, internal/ingestor/push/rtmp_server.go:444

## Hiện tượng
Sau khi `POST /config` đổi `listeners.rtmp` hoặc section `ingestor`:
- RTMP play bị từ chối với "play handler not configured".
- Push vào path auto-publish bị từ chối.
- Stream có `stream_key` chấp nhận push không cần khoá.
- Encoder kết nối mới vào stream `publish://` đang chạy không cập nhật manager: input timeout, stream Degraded hoặc exhausted, dù dữ liệu vẫn vào buffer.

## Điều kiện kích hoạt
`diffService("ingestor")` restart service do đổi port/host RTMP hoặc đổi `Ingestor`.

## Phân tích
Mỗi `Run` tạo một `RTMPServer` mới, nhưng chỉ nạp từ các trường `pending*`, vốn đã được xoá ở lần chạy đầu:
```go
if s.pendingPlayFunc != nil {
    rtmpSrv.SetPlayFunc(s.pendingPlayFunc)
    s.pendingPlayFunc = nil
}
```
Cấu hình đặt sau đó đi thẳng vào server cũ, `Ingestor` không lưu lại bản nào. `runtime.diff` không gọi lại `SetRTMPPlayHandler` hoặc `SetAutoPublishResolver`. `pendingPushCallbacks` cũng nil nên server mới không có callback của các stream đang chạy.

Hậu quả xác thực: `pushKeyOK` có `if fn == nil { return true }`, tức resolver mất đồng nghĩa tắt kiểm tra khoá.

## Vì sao đây là lỗi
Comment trong `SetListeners` mô tả restart nóng là luồng được hỗ trợ. Thay đổi cấu hình listener âm thầm tắt xác thực push là hành vi fail-open.

## Hướng sửa
Lưu bền `playFunc`, `resolver`, `streamKeyFn` và bản đồ callback trên `ingestor.Service`, áp lại cho mỗi `RTMPServer` mới. Xử lý luôn các pub session trên server cũ.

## Cách xác minh
Unit test: gọi `Run`, cancel, gọi `Run` lần hai và kiểm `playFunc`, `resolver`, `streamKeyFn`, callback trên `s.rtmpSrv`. Trên môi trường thử: đổi port RTMP và thử push sai `?key=`.
