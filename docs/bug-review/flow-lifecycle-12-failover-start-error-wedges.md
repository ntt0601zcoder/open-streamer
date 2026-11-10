# Failover sang input lỗi đồng bộ làm stream kẹt vĩnh viễn mà không báo Degraded

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/manager, internal/ingestor, internal/coordinator, internal/vod
- **Vị trí:** internal/manager/service.go:932-939, internal/manager/service.go:880-882, internal/manager/service.go:816-823, internal/ingestor/service.go:477-480, internal/ingestor/reader.go:120-123, internal/ingestor/pull/copy.go:86-89

## Hiện tượng
Input chính chết, backup là input không tạo được reader. Stream không có dữ liệu nhưng status vẫn Active, không `exhausted`, không có sự kiện hết input, không còn thử lại, reconciler không can thiệp, kể cả khi input chính đã phục hồi.

## Điều kiện kích hoạt
1. `inputs = [hls://... (0), file://mount-đã-xoá/x hoặc copy://stream-đã-xoá (1)]`.
2. Input 0 kết thúc bằng EOF hoặc lỗi fatal (HTTP 404, TLS...), worker return.
3. `tryFailover` chọn input 1, `ingestor.Start` lỗi ở `NewPacketReader`.

## Phân tích
Đường lỗi chỉ log và return:
```go
if err := s.ingestor.Start(ctx, streamID, bestInput, bufID); err != nil {
    slog.Error(...)
    return
}
```
Input 1 vẫn Idle, `state.active` vẫn là 0, `exhausted=false`. Không vòng nào thử lại:
- `collectTimeoutIfNeeded` bỏ qua vì input 0 đã Degraded.
- `collectProbeIfNeeded` bỏ qua vì `priority == state.active && !state.exhausted`.
- `shouldFailbackNow` yêu cầu `best.Priority < state.active`, mà best là 1.
- Không gọi `handleExhausted` nên coordinator không biết.

## Vì sao đây là lỗi
Logic probe giả định "live worker đang tự reconnect" khi active chưa exhausted, nhưng worker đã thoát. `Register` đã có xử lý đúng cho lỗi đồng bộ (A-5: `ReportInputError`), còn `tryFailover` thì chưa.

## Hướng sửa
Khi `Start` lỗi: đánh dấu input đích Degraded với error history, rồi thử ứng viên tiếp theo hoặc vào `handleExhausted`. Lặp có giới hạn để không đệ quy vô hạn.

## Cách xác minh
Test manager với ingestor stub: `Start` lỗi cho priority 1, gọi `ReportInputError(0, io.EOF)`, khẳng định input 1 Degraded và `Exhausted=true`, callback được gọi, và probe được xếp lịch.
