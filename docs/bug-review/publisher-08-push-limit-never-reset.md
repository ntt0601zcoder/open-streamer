# Bộ đếm retry của push không reset sau phiên thành công
- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** publisher (RTMP push), domain
- **Vị trí:** internal/publisher/push_rtmp.go:399-429; internal/domain/push.go:33-34

## Hiện tượng
Một đích push có `limit` chạy ổn định nhiều ngày, thỉnh thoảng rớt mạng rồi tự nối lại, sau đó đột nhiên dừng hẳn dù đích vẫn sống. Muốn chạy lại phải sửa cấu hình hoặc restart stream.

## Điều kiện kích hoạt
`Limit = N > 0`, và tổng số lần rớt phiên kể từ khi goroutine chạy đạt N, dù các phiên giữa chúng hoạt động tốt.

## Phân tích
`attempts` chỉ về 0 khi có `errDiscontinuity`. Phiên kết nối thành công rồi rớt vì lỗi mạng không reset:
```go
attempts++
sessionErr := s.runOnePushSession(ctx, streamID, mediaBufferID, dest)
if errors.Is(sessionErr, errDiscontinuity) { attempts = 0; ...; continue }
```
Lần kết nối đầu cũng bị tính, nên `Limit=1` nghĩa là không có retry nào.

## Vì sao đây là lỗi
`domain.PushDestination.Limit` ghi "maximum number of retry attempts", tức giới hạn cho các lần thử liên tiếp thất bại. Stream chạy dài sẽ tích lũy dần và chết, trong khi `setPushStatus(Active)` đã xóa lịch sử lỗi như thể đích đã khỏe.

## Hướng sửa
Reset `attempts` khi phiên đã đạt `Active` (hoặc chạy đủ một khoảng tối thiểu), và tính Limit theo số retry sau lần thử đầu.

## Cách xác minh
Test giả lập phiên connect OK rồi lỗi N lần: goroutine phải tiếp tục thử. Thực tế: `limit: 2`, khởi động lại server đích ba lần.
