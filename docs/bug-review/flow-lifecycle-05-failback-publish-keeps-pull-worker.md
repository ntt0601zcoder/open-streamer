# Pusher và pull backup cùng ghi vào một buffer, pull worker không dừng sau failback

- **Mức độ:** Nghiêm trọng
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/manager, internal/ingestor, internal/ingestor/push, internal/buffer
- **Vị trí:** internal/ingestor/service.go:356-360, internal/ingestor/service.go:619-659, internal/ingestor/registry.go:46-50, internal/ingestor/push/rtmp_server.go:598, internal/manager/service.go:694-706, internal/manager/service.go:932-941

## Hiện tượng
Stream có input chính `publish://` và backup pull: khi encoder kết nối, hình và tiếng từ hai nguồn xen kẽ trong cùng buffer, player bị vỡ hình hoặc nhảy nội dung. Sau failback về push, backup vẫn tiếp tục chạy và tiếp tục ghi.

## Điều kiện kích hoạt
1. Stream có `inputs = [publish:// (0), rtmp://backup (1)]`. Lúc boot chưa có encoder nên `ErrNoPusherConnected` đẩy manager sang input 1 và pull worker chạy. Cảnh này cũng xảy ra khi encoder rớt rồi quay lại.
2. Encoder kết nối: slot trong registry vẫn còn nên `Acquire` thành công, và `pubState` ghi thẳng vào buffer trong khi worker input 1 đang ghi.
3. `RecordPacket(0)` đưa input 0 về Active, `shouldFailbackNow` gọi `tryFailover` về 0.

## Phân tích
Nhánh push của `Ingestor.Start` không chạm vào `s.workers`:
```go
if protocol.IsPushListen(input.URL) {
    return s.startPushRegistration(streamID, input, bufferWriteID)
}
```
Nên worker của input 1 không bị cancel; chỉ `Stop` hoặc lần `startPullWorker` sau mới huỷ nó.

`startPushRegistration` còn làm hai việc sai khi encoder đang kết nối. `registry.Register` ghi đè entry và xoá cờ `active`, cho phép encoder thứ hai `Acquire`. Nó cũng báo `ErrNoPusherConnected`; với nhiều input, `ReportInputError` đánh dấu input 0 Degraded dù đang có packet, rồi `commitSwitch` vẫn đặt `active=0`.

## Vì sao đây là lỗi
Kiến trúc yêu cầu một ingestor đang hoạt động ghi vào buffer ("Only the active Ingestor goroutine for this stream should call Write"). Handoff chỉ được cài cho đường pull-sang-pull. Manager test dùng ingestor stub nên không phát hiện được. Cả hai nguồn đều được neo PTS theo wallclock nên timestamp trông hợp lệ và không có guard nào loại bỏ.

## Hướng sửa
Khi chuyển sang push: cancel pull worker hiện tại (có thể sau packet đầu của pusher). Chỉ cho pusher ghi khi input đó đang active, hoặc thay bằng cơ chế gate. Không reset `active` và không báo `ErrNoPusherConnected` khi slot đang có pusher.

## Cách xác minh
Integration test với ingestor thật: đăng ký `publish://` và pull giả, gọi `Start` input 1, rồi `Start` input 0 và khẳng định worker 1 đã bị cancel. Trên môi trường thử: đếm `SessionStart` và số nguồn packet trong buffer sau khi encoder kết nối.
