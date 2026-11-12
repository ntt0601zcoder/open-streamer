# Viewer kết nối dài biến mất vĩnh viễn khỏi tracker nếu nguồn ngừng quá idle timeout

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/sessions, internal/publisher
- **Vị trí:** internal/publisher/sessions_helper.go:43-49, internal/publisher/sessions_helper.go:63-82, internal/sessions/reaper.go:77-79, internal/sessions/tracker.go:463-473, internal/publisher/serve_mpegts.go:168-198

## Hiện tượng
Khi nguồn mất hơn 30 giây (mặc định) rồi hồi phục, viewer RTMP, SRT, MPEG-TS vẫn kết nối và xem tiếp nhưng không còn trong `/sessions`. Metric active giảm sai, hook nhận `session.closed reason=idle` dù viewer chưa rời, và số byte về sau không được tính.

## Điều kiện kích hoạt
Viewer connection-bound đang kết nối, stream không có packet trong thời gian dài hơn `idle_timeout_sec` (mọi input lỗi, failover chậm), pipeline vẫn tồn tại, sau đó packet quay lại.

## Phân tích
`Touch` chỉ xảy ra trong đường ghi dữ liệu (`add`). Khi không có packet thì không có `add`, nên reaper đóng session:
```go
if sess.UpdatedAt.Before(idleCutoff) {
    victims = append(victims, victim{id, domain.SessionCloseIdle})
```
`closeByIDCtx` xoá record khỏi map. Khi dữ liệu trở lại, `touchByID` bỏ qua vì id không còn, và không có đường nào mở lại session. RTSP có ticker riêng nên không bị.

## Vì sao đây là lỗi
Comment trong reaper mô tả việc reap session kết nối chỉ là lưới an toàn cho "panic, ctx race" khi đường close bị bỏ lỡ. Ở đây transport vẫn sống và nguyên nhân là phía nguồn.

## Hướng sửa
Thêm heartbeat theo ticker trong vòng phục vụ, độc lập với luồng dữ liệu, như RTSP; hoặc cho reaper bỏ qua session connection-bound trừ khi không có heartbeat.

## Cách xác minh
Test: mở `OpenConn`, không `Touch`, chạy `reapOnce` với đồng hồ giả, rồi `Touch`, kiểm tra `Get` trả `false` trong khi closer chưa được gọi.
