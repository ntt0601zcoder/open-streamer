# Client HTTP MPEG-TS bị cắt đúng sau 120 giây

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api, internal/publisher
- **Vị trí:** internal/api/server.go:203, internal/api/dispatch.go:171-177, internal/publisher/serve_mpegts.go:124, internal/publisher/serve_mpegts.go:168-171

## Hiện tượng
Mọi kết nối `GET /<code>/mpegts` (ffmpeg, VLC, Open-Streamer khác làm relay) đều đứt sau đúng 120 giây dù stream khoẻ. Relay phải reconnect liên tục, mỗi lần gây discontinuity.

## Điều kiện kích hoạt
Bật MPEG-TS cho stream, mở `/<code>/mpegts` và giữ quá 120 giây.

## Phân tích
Router gốc áp timeout cho mọi route, gồm cả catch-all media:
```go
r.Use(middleware.Timeout(120 * time.Second))
```
Handler dùng chính context đó làm điều kiện dừng:
```go
streamMPEGTSToClient(r.Context(), code, sub, w, flusher, mpegtsSess)
...
case <-ctx.Done():
    return
```
Sau khi handler trả về, `middleware.Timeout` còn gọi `w.WriteHeader(504)` trên response đã gửi, sinh log superfluous.

## Vì sao đây là lỗi
Comment của `HandleMPEGTS` nói handler chạy "until the client disconnects, the stream stops, or the request context is cancelled", và đặt endpoint này làm đường relay server-to-server dài hạn. Không có route nào được miễn timeout.

## Hướng sửa
Bỏ `Timeout` ở router gốc và chỉ áp cho nhóm admin, hoặc miễn route `mpegts`. Trong handler có thể dùng context tách khỏi deadline nhưng vẫn theo lúc client ngắt.

## Cách xác minh
Test `httptest` dựng router có `middleware.Timeout(200ms)` bao `HandleMPEGTS`, kiểm tra handler trả về lúc deadline. Thủ công trên môi trường riêng: `curl -o /dev/null -w '%{time_total}' http://host/<code>/mpegts`, kết quả khoảng 120 giây.
