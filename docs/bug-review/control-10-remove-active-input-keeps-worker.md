# Xoá input đang active khi không còn candidate, worker cũ vẫn ghi vào stream
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** coordinator, manager, ingestor
- **Vị trí:** internal/manager/service.go:1179-1187, 1215-1216, 904-909, 608; internal/coordinator/coordinator.go:569-575, 599-601; internal/domain/stream.go:121-133

## Hiện tượng
Operator xoá nguồn đang phát (ví dụ nguồn sai bản quyền) bằng `PUT` với `inputs: []`, hoặc xoá input active khi các input còn lại đều Degraded. API trả 200, `runtime.inputs` không còn input đó, nhưng người xem vẫn thấy nội dung từ URL đã xoá. Stream báo `degraded`.

## Điều kiện kích hoạt
Một trong hai, trên stream thường (không phải ABR copy/mixer):
1. `PUT {"inputs": []}`. `ValidateInputPriorities` chấp nhận danh sách rỗng.
2. Inputs `[0: A đang active, 1: B đã Degraded]`, `PUT` xoá A.

## Phân tích
`UpdateInputs` xoá khỏi map rồi gọi `tryFailover`. Không có candidate thì:
```go
if best == nil {
    s.handleExhausted(streamID, state)
    return
}
```
Không có lời gọi `ingestor.Stop`, nên worker của URL đã xoá vẫn chạy và ghi vào buffer. `RecordPacket` cho priority đã xoá không tìm thấy entry nên packet bị bỏ qua về mặt health. Vì `IsRunning` vẫn true nên reconciler không can thiệp, và `state.active` trỏ tới priority không còn tồn tại.

## Vì sao đây là lỗi
Nhánh ABR trong `updateLocked` có xử lý rõ `len(new.Inputs) == 0` bằng cách dừng pipeline. Nhánh thường không có xử lý tương đương. Việc xoá nguồn là ý định rõ ràng của operator, và comment trong `startPullWorker` cũng nêu nguyên tắc không để config cũ tiếp tục đẩy dữ liệu.

## Hướng sửa
Trong `UpdateInputs`, nếu input active bị xoá và không chọn được input thay thế, gọi `ingestor.Stop(streamID)`. Với `len(new.Inputs) == 0`, `updateLocked` nên `stopLocked` như nhánh ABR.

## Cách xác minh
Test manager: Register một input, `UpdateInputs(removed=[0])`, kiểm tra stub ingestor nhận `Stop`. Hiện tại `Stop` không được gọi.
