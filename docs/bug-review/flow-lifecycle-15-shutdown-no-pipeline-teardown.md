# Shutdown không dừng pipeline, để lại subprocess mồ côi và DVR chưa seal

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** cmd/server, internal/runtime, internal/api, internal/coordinator, internal/transcoder, internal/dvr/blob
- **Vị trí:** cmd/server/main.go:181-193, internal/runtime/manager.go:124-135, internal/api/handler/stream.go:433, internal/api/handler/stream.go:779, internal/transcoder/service.go:317, internal/transcoder/supervisor.go:159, internal/dvr/blob/service.go:106, cmd/open-streamer-transcoder/main.go (hàm `run`), internal/transcoder/native/server.go:88-89

## Hiện tượng
SIGTERM với stream được tạo hoặc restart qua API: sau khi server thoát, `open-streamer-transcoder` vẫn tồn tại kèm file socket trong `/tmp`, tích luỹ qua nhiều lần restart nếu không chạy dưới cgroup dọn process. Hour DVR đang ghi không được seal, sự kiện `stream.stopped` không phát.

## Điều kiện kích hoạt
1. Stream do `POST /streams`, restart, `PUT`, template hoặc config YAML khởi động (ctx `WithoutCancel`).
2. Server nhận SIGTERM và không bị service manager kill cả nhóm process.

## Phân tích
`main` chỉ cancel rootCtx, gọi `injector.ShutdownWithContext`, rồi return:
```go
<-ctx.Done()
...
if err := injector.ShutdownWithContext(shutdownCtx); err != nil {
```
Không service nào đăng ký Shutdown, không có vòng `Coordinator.Stop`, `Manager.WaitAll` không được gọi.

Context của stream API tách khỏi rootCtx nên `exec.CommandContext(subCtx, ...)` không bao giờ kill. Stream từ bootstrap có rootCtx nhưng main thoát ngay, nên việc kill chạy đua với lúc process kết thúc. Không có `Pdeathsig`. Khi cha chết, gRPC handler `Run` return lỗi, nhưng `main` của subprocess chỉ thoát theo tín hiệu hoặc lỗi `Serve`, nên process ở lại vô thời hạn.

DVR dùng `context.WithoutCancel`, `StopRecording` không chạy nên `w.Close` không seal. `RepairStream` chỉ phục hồi ở lần boot sau.

## Vì sao đây là lỗi
Tài liệu mô tả "Graceful shutdown" và mọi goroutine phải có đường cancel rõ ràng. Hai nguồn ctx khác nhau làm hành vi shutdown phụ thuộc cách stream được tạo.

## Hướng sửa
Trong `main`, trước khi thoát: duyệt `RunningStreams()` và `Stop` có timeout, chờ `rtm.WaitAll`. Trên Linux đặt `Pdeathsig: SIGKILL` cho subprocess, và cho subprocess thoát khi `Run` kết thúc.

## Cách xác minh
Trên môi trường thử riêng: tạo stream transcode qua API, SIGTERM server, rồi `pgrep open-streamer-transcoder` và kiểm tra `/tmp/ost-*.sock`.
