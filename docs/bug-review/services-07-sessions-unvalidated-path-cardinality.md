# Request media không tồn tại tạo session, event và series Prometheus không giới hạn

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/sessions, internal/metrics, internal/hooks, internal/api
- **Vị trí:** internal/sessions/httpmw.go:31-55, internal/sessions/httpmw.go:75-86, internal/sessions/tracker.go:577-605, internal/metrics/metrics.go:355-368, internal/api/server.go:320-325

## Hiện tượng
Một scanner hoặc script quét `/<ngẫu nhiên>/a.ts` làm `/metrics` phình mãi, số series `open_streamer_sessions_*` tăng theo từng path. `/sessions` hiện viewer của stream không tồn tại. Hook nhận `session.opened/closed` giả. Request bị 403 vẫn được tính là viewer.

## Điều kiện kích hoạt
`sessions.enabled=true`, gửi không cần xác thực các request như `GET /x1/a.ts`, `GET /x2/a.ts`, ... Bộ nhớ Prometheus không trở lại mức cũ cho tới khi restart.

## Phân tích
Middleware bao toàn bộ catch-all và lấy code từ URL thô, sau khi handler đã chạy xong bất kể kết quả:
```go
code := streamCodeFromPath(r.URL.Path)
...
next.ServeHTTP(cw, r)
...
t.TrackHTTP(r.Context(), hit)
```
`TrackHTTP` mở session mới cho mỗi fingerprint, rồi `observeOpened` tạo series với `stream_code` là chuỗi do client gửi. `observeClosed` chỉ `Dec()` và `Inc()`; không có `DeleteLabelValues` nào cho nhóm sessions.

## Vì sao đây là lỗi
Comment "Non-2xx responses still record a hit" chỉ nhằm hiện viewer gặp segment hỏng của stream thật. Không có kiểm tra stream tồn tại hay kết quả `mediaAllowed`. `mediaAllowed` cũng chạy bên trong middleware, nên request đã bị từ chối vẫn được ghi nhận. Lưu lượng này ảnh hưởng cả queue 512 của event bus, đẩy event thật ra khỏi hàng đợi.

## Hướng sửa
Chỉ track khi code hợp lệ và stream đang chạy hoặc có archive, và response không phải 401/403/404. Có thể chuyển việc track vào `dispatchMedia` sau khi authorize. Xoá series khi stream dừng.

## Cách xác minh
Test: gọi middleware với nhiều path không tồn tại kèm handler trả 404, kiểm tra `tracker.Stats().OpenedTotal` tăng và `testutil.CollectAndCount(SessionsActive)` tăng tương ứng.
