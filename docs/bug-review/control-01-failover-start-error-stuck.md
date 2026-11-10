# Failover dừng hẳn khi không dựng được reader cho input dự phòng tốt nhất
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** manager, ingestor
- **Vị trí:** internal/manager/service.go:932-939, 847-849, 880-882, 816-823, 1147-1148; internal/ingestor/service.go:476-480; internal/ingestor/reader.go:116-123, 134, 143-151; internal/domain/stream.go:121-148

## Hiện tượng
Input chính chết, log có `manager: failed to start new ingestor` đúng một lần, rồi stream đứng hình vô thời hạn. Các input dự phòng tốt hơn ở phía sau không bao giờ được thử. API vẫn báo `active` (không `exhausted`), `active_input_priority` vẫn là input đã chết. `POST /streams/{code}/switch` trong tình huống này vẫn trả `switched` dù không chuyển gì.

## Điều kiện kích hoạt
1. Stream có inputs `[0: nguồn pull bình thường, 1: URL không dựng được reader, 2: nguồn tốt]`. URL của input 1 có thể là `file://` trỏ tới mount đã xoá, `copy://` tới stream đã xoá, hoặc scheme gõ sai. API không kiểm tra scheme.
2. Input 0 lỗi non-retriable (HTTP 404/503, EOF) hoặc timeout.

## Phân tích
`startPullWorker` trả lỗi ngay khi `NewPacketReader` lỗi. `tryFailover` chỉ log rồi return:
```go
if err := s.ingestor.Start(ctx, streamID, bestInput, bufID); err != nil {
    slog.Error("manager: failed to start new ingestor", ...)
    return
}
```
Input 1 vẫn `Idle`, không bị đánh dấu Degraded, nên `selectBest` sẽ luôn chọn lại nó và không bao giờ tới input 2. Sau lần lỗi đó không còn đường nào gọi lại `tryFailover`:
- `collectTimeoutIfNeeded` bỏ qua active đã Degraded.
- `collectProbeIfNeeded` bỏ qua active khi `!state.exhausted`.
- `shouldFailbackNow` chỉ true khi `best.Priority < state.active`; ở đây best=1, active=0.

`state.exhausted` cũng không được đặt vì `best != nil`.

## Vì sao đây là lỗi
Fix A-5 trong `Register` đã đưa lỗi start đồng bộ về `ReportInputError`, nhưng `tryFailover` và `UpdateInputs` không có xử lý tương tự. Worker của input 0 đã thoát trên EOF hoặc lỗi fatal, nên cũng không tự hồi phục. `SwitchInput` đặt override rồi trả `nil` bất kể `tryFailover` có thành công hay không.

## Hướng sửa
Khi `ingestor.Start` lỗi, đánh dấu input đích Degraded (ghi error, `degradedAt`) rồi chạy lại vòng chọn input, để đi tiếp tới candidate sau hoặc vào `exhausted`. `tryFailover` nên trả lỗi để `SwitchInput` báo về API.

## Cách xác minh
Test với stub ingestor: `Start` lỗi cho priority 1, thành công cho priority 2. Gọi `ReportInputError(0)` rồi kiểm tra active chuyển sang 2. Hiện tại active giữ nguyên 0 sau mọi tick `checkHealth`.
