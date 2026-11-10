# Lệnh Stop không bao giờ tới subprocess nên encoder tail luôn bị mất

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder, internal/transcoder/native
- **Vị trí:** internal/transcoder/supervisor.go:156-160, internal/transcoder/supervisor.go:200, internal/transcoder/supervisor.go:251-258, internal/transcoder/supervisor.go:262-269, internal/transcoder/native/server.go:139-140, internal/transcoder/native/server.go:153-166

## Hiện tượng
Khi dừng stream hoặc reload transcoder, GOP cuối, audio đang giữ và frame B trong encoder bị bỏ. Segment cuối thiếu dữ liệu. Subprocess luôn bị SIGKILL, kể cả NVENC, thay vì tắt sạch.

## Điều kiện kích hoạt
Mọi lần `Service.Stop`.

## Phân tích
Cả subprocess và gRPC stream đều gắn với `subCtx`:

```go
cmd := exec.CommandContext(subCtx, binPath, "--socket", socketPath)
stream, err := client.Run(subCtx)
```

Stop chỉ được gửi sau khi context đã hủy:

```go
case <-subCtx.Done():
    _ = stream.Send(&pb.Request{Body: &pb.Request_Stop{...}})
    _ = stream.CloseSend()
```

Lúc đó `stream.Send` thất bại vì stream đã hủy, và `exec.CommandContext` đã giết process. `gracefulStopTimeout` trên thực tế không có tác dụng ở đường này. Lệnh gửi này còn chạy đồng thời với `stream.Send` trong `inputPump`, trong khi gRPC không cho phép nhiều goroutine cùng gửi trên một stream.

## Vì sao đây là lỗi
Comment nói Stop gửi best-effort "so the subprocess flushes its encoder tail", và `Flush` có nhiều logic để giữ phần đuôi, nhưng đường production không bao giờ đến đó.

## Hướng sửa
Tách context của process và stream khỏi context dừng. Khi `ctx` bị hủy: dừng `inputPump`, gửi Stop từ duy nhất một goroutine gửi, chờ `outputDrain` với timeout, rồi mới hủy context và kill.

## Cách xác minh
Test tích hợp với server giả, gọi cancel và kiểm tra server có nhận `Stop` hay không. Hoặc thêm log trong `flushAndSend` và dừng một stream.
