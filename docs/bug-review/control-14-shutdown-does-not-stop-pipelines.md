# Shutdown không dừng pipeline và không chờ service, transcoder có thể mồ côi
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** cmd/server, runtime, coordinator, transcoder, dvr/blob, hooks
- **Vị trí:** cmd/server/main.go:183-193; internal/runtime/manager.go:124-135; internal/api/handler/stream.go:433, 779; internal/manager/service.go:383; internal/transcoder/service.go:317; internal/transcoder/supervisor.go:159; cmd/open-streamer-transcoder/main.go:91-110; internal/dvr/blob/service.go:106, 137-155; internal/hooks/service.go:226-242

## Hiện tượng
Sau SIGTERM:
- Tiến trình `open-streamer-transcoder` của các stream được tạo hoặc restart qua API có thể còn sống, khi server không chạy trong cgroup bị kill toàn bộ (`make run`, nohup, một số cách chạy container).
- Recording DVR không được seal và catalog không được lưu lần cuối. Phần cuối phụ thuộc vào `RepairStream` ở lần chạy sau.
- Batch hook đang chờ có thể mất.

## Điều kiện kích hoạt
Gửi SIGINT hoặc SIGTERM khi có stream đang chạy, đặc biệt là stream đã qua `POST /restart` hoặc `PUT`.

## Phân tích
`main` chỉ làm:
```go
<-ctx.Done()
...
if err := injector.ShutdownWithContext(shutdownCtx); err != nil {
```
Không service nào cài `do.Shutdowner` (grep chỉ thấy `http.Server.Shutdown`), và `runtime.Manager.WaitAll` không có nơi gọi. Vì vậy `main` return gần như ngay lập tức, không chờ gì. Không có lời gọi `Coordinator.Stop` cho từng stream.

Ngoài ra pipeline start qua API dùng `context.WithoutCancel`, nên không bị rootCtx huỷ. Với transcoder, `exec.CommandContext(subCtx, ...)` không bao giờ kill trong trường hợp này. Subprocess không dùng `Pdeathsig` và vòng chính chỉ thoát khi nhận signal hoặc `Serve` lỗi, nên khi stream gRPC đứt nó vẫn listen tiếp.

Với stream khởi động lúc boot (rootCtx), lệnh kill chạy trong goroutine của `exec`, đua với `main` thoát. DVR luôn dùng `WithoutCancel` nên `w.Close` và `cat.Save` không chạy. Hooks có drain sau `ctx.Done()`, nhưng không ai chờ nó.

## Vì sao đây là lỗi
`main` log "shutdown complete" và đặt timeout 30 giây cho việc không làm gì. Comment ở hooks mô tả "graceful shutdown loses minimal data". `StopRecording` có ghi rõ phải seal hour đang ghi. `EventStreamStopped` cũng không phát khi shutdown.

## Hướng sửa
Sau `<-ctx.Done()`: dừng reconciler, gọi `Coordinator.Stop` cho mọi `RunningStreams()` với deadline, rồi `rtm.WaitAll()` có timeout. Bổ sung `Pdeathsig` (Linux) hoặc cho subprocess tự thoát khi stream gRPC đứt.

## Cách xác minh
Chạy server trên môi trường thử ngoài cgroup-kill, tạo stream transcode qua API, gửi SIGTERM rồi `pgrep open-streamer-transcoder`. Kiểm tra catalog DVR có được cập nhật lần cuối hay không.
