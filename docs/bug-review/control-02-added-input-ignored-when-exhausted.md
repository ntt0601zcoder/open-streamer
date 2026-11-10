# Thêm input dự phòng khi stream đã exhausted không có tác dụng
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** manager, coordinator
- **Vị trí:** internal/manager/service.go:1189-1199, 1215-1225, 808-815, 877-882; internal/domain/stream.go:119-133

## Hiện tượng
Mọi input đều chết, stream `Degraded`, `exhausted=true`. Operator thêm một nguồn dự phòng còn sống bằng `PUT /streams/{code}`. API trả 200, input mới hiện `idle`, nhưng stream không chuyển sang nó và tiếp tục mất hình cho tới khi `POST /restart` hoặc input cũ tự hồi phục.

## Điều kiện kích hoạt
1. Stream chạy với inputs `[0: A]`, A chết, stream vào `exhausted`.
2. `PUT` thành `[0: A, 1: B]`, với B đang sống.

## Phân tích
`ValidateInputPriorities` buộc priority liền nhau `0..N-1`, nên input thêm vào cuối luôn có priority lớn hơn active. `UpdateInputs` chỉ failover khi input mới có độ ưu tiên cao hơn active:
```go
if inp.Priority < state.active && failoverReason == "" {
    failoverReason = SwitchReasonInputAdded
}
```
Sau đó `failoverReason == ""`, không có `restartInput`, nên không gọi `tryFailover`. Các đường còn lại cũng không chạm tới B:
- `shouldFailbackNow` return false khi `state.exhausted`.
- Probe chỉ chạy cho input `Degraded`; B đang `Idle`.
- `RecordPacket` chỉ đến từ worker đang chạy, mà B chưa có worker.

## Vì sao đây là lỗi
`selectBest` lúc này sẽ trả về B và `tryFailover` xử lý được đúng trường hợp `exhausted`, nhưng không có ai gọi nó. Test hiện có (`TestUpdateInputs_AddsNewInputAndFailsOverWhenHigherPriority`) chỉ phủ trường hợp priority cao hơn.

## Hướng sửa
Trong `UpdateInputs`, khi `state.exhausted` và có input được thêm hoặc cập nhật, gọi `tryFailover` với reason `input_added` (hoặc `recovery`). Hoặc cho sweeper trong `checkHealth` thử lại khi `exhausted && selectBest != nil`.

## Cách xác minh
Test: Register một input, `ReportInputError` để exhausted, `UpdateInputs(added=[priority 1])`, chạy `checkHealth`. Kỳ vọng `ingestor.Start` được gọi với priority 1 và `exhausted=false`. Hiện tại không có lần Start nào.
