# Đổi cấu hình listener RTSP làm cả server panic

- **Mức độ:** Nghiêm trọng
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/runtime, internal/publisher, internal/api
- **Vị trí:** internal/publisher/serve_rtsp.go:127-132, internal/publisher/serve_rtsp.go:172, internal/publisher/service.go:184, internal/runtime/manager.go:293-300, internal/runtime/manager.go:432-435, internal/runtime/manager.go:454-459

## Hiện tượng
`POST /config` đổi port hoặc host RTSP (hoặc tắt rồi bật lại RTSP) làm process chết với `panic: close of closed channel`. Mọi stream và viewer trên máy bị rớt.

## Điều kiện kích hoạt
1. Server boot với `listeners.rtsp.enabled=true`, `RunRTSPPlayServer` đã đóng `rtspSrvReady`.
2. Operator gửi `POST /config` hoặc `PUT /config/yaml` đổi `listeners.rtsp`.
3. `diffService("pub_rtsp")` stop rồi start lại service, `RunRTSPPlayServer` chạy lần hai.

## Phân tích
`rtspSrvReady` chỉ được tạo một lần trong constructor, nhưng mỗi lần chạy lại đều đóng nó:
```go
s.rtspSrv = srv
s.mu.Unlock()
close(s.rtspSrvReady)
```
Hàm chạy trong goroutine của `Manager.startService`, không có `recover`, nên panic hạ toàn bộ process. Vòng tắt rồi bật cũng dính: lần bật đầu đóng channel, lần bật sau đóng lại.

## Vì sao đây là lỗi
`runtime.diff` được thiết kế để restart listener nóng ("SetListeners then restart"), nhưng `RunRTSPPlayServer` chỉ an toàn cho một lần gọi trong đời process. Không có test nào gọi hàm này hai lần.

## Hướng sửa
Đóng channel qua `sync.Once`, hoặc tạo lại channel cho mỗi lần chạy dưới `s.mu`. Khi restart cần xử lý thêm các `serveRTSP` đang giữ `*gortsplib.Server` cũ, và đặt `s.rtspSrv = nil` khi server dừng.

## Cách xác minh
Unit test: gọi `RunRTSPPlayServer(ctx)` với port trống, cancel, gọi lần hai và quan sát panic. Trên môi trường thử riêng: bật RTSP, `POST /config` đổi port.
