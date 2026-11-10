# Xoá hết input khỏi stream đang chạy không dừng ingest

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/coordinator, internal/manager, internal/ingestor
- **Vị trí:** internal/api/handler/stream.go:685-690, internal/coordinator/coordinator.go:599-601, internal/manager/service.go:1179-1187, internal/manager/service.go:904-909, internal/manager/service.go:962-977, internal/coordinator/coordinator.go:320-322, internal/coordinator/coordinator.go:958-960

## Hiện tượng
Operator gỡ nguồn khỏi stream bằng `"inputs": []`, ví dụ vì nguồn vi phạm bản quyền. API trả 200 và bản ghi không còn input, trạng thái chuyển Degraded, nhưng worker vẫn kéo nguồn và người xem vẫn thấy hình.

## Điều kiện kích hoạt
Stream pull một input đang chạy, `POST /streams/{code}` với `{"inputs":[]}`. Hoặc template bỏ hết `inputs` khi stream kế thừa từ đó.

## Phân tích
`ValidateInputPriorities` và `ValidateUniqueInputs` đều qua với mảng rỗng. Đường thường trong `updateLocked` không có kiểm tra `len(new.Inputs) == 0`, khác nhánh ABR. `UpdateInputs` xoá khỏi map rồi gọi `tryFailover`. `selectBest` trả nil:

```go
if best == nil {
    s.handleExhausted(streamID, state)
    return
}
```

`handleExhausted` chỉ đặt cờ và gọi callback, không hề `s.ingestor.Stop`. Pull worker trong `ingestor.workers` chạy tiếp và ghi buffer. `RecordPacket` không tìm thấy input đã xoá, nên trạng thái càng lệch thực tế. `/restart` sau đó dừng stream và trả lỗi "has no inputs". Reconciler cũng bỏ qua vì không còn input.

## Vì sao đây là lỗi
Comment của `UpdateInputs` nói input đang active bị xoá sẽ failover, nhưng không có đường xử lý khi không còn gì để failover. Runtime tiếp tục phát từ nguồn đã bị gỡ khỏi cấu hình.

## Hướng sửa
Khi `new.Inputs` rỗng trong `updateLocked`, gọi `stopLocked` như nhánh ABR. Trong `UpdateInputs`, nếu active bị xoá và không có lựa chọn thay thế thì gọi `s.ingestor.Stop`.

## Cách xác minh
Test coordinator với spy manager, và test manager với fake ingestor: xoá input duy nhất phải dẫn tới `Stop`.
