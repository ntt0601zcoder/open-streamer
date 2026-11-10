# Callback exhausted và restored có thể đảo thứ tự, stream kẹt Degraded
- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** manager, coordinator
- **Vị trí:** internal/manager/service.go:971-976, 1015-1020; internal/coordinator/coordinator.go:798-818

## Hiện tượng
Nguồn chập chờn rất ngắn đúng lúc timeout. Sau đó stream phát bình thường và `runtime.exhausted=false`, nhưng `status` vẫn `degraded` cho tới khi có một chu kỳ exhausted/restored mới hoặc restart stream.

## Điều kiện kích hoạt
Chuyển `exhausted` → `restored` xảy ra trong khoảng vài micro giây tới vài mili giây. Ví dụ: `checkHealth` đánh dấu timeout và exhausted cho stream một input, ngay sau đó packet đến và `RecordPacket` xoá exhausted.

## Phân tích
Cả hai callback đều chạy trong goroutine riêng, không có thứ tự:
```go
go cb(streamID)
```
Hai handler của coordinator đều log rồi mới `updateDegradation`. Nếu goroutine restored chạy trước, cờ được xoá trước, rồi goroutine exhausted đặt `inputsExhausted=true`. Manager lúc này đã có `exhausted=false`, nên không còn callback restored nào nữa.

## Vì sao đây là lỗi
Comment ở `ReportInputError` đã ghi nhận loại race này ("under adverse ordering the Degraded flag stays set") nhưng chỉ tránh cho `ErrNoPusherConnected` một input. `Go` không đảm bảo thứ tự giữa các goroutine.

## Hướng sửa
Gửi trạng thái theo thứ tự qua một queue theo stream, hoặc để coordinator đọc lại trạng thái hiện tại của manager trong callback thay vì tin vào hướng của sự kiện.

## Cách xác minh
Khó tái hiện tự nhiên. Có thể dựng test với `onExhausted` chặn bằng channel, chạy restored trước rồi thả exhausted, và kiểm tra `StreamStatus` lệch với `RuntimeStatus.Exhausted`.
