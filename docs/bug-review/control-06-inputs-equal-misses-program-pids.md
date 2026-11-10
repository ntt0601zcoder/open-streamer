# Diff input bỏ sót Program và Pids, đổi cấu hình lọc UDP không được áp dụng
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** coordinator, manager, ingestor
- **Vị trí:** internal/coordinator/diff.go:98-100, 233-238; internal/domain/input.go:63, 78; internal/ingestor/reader.go:100-103, 162-178; internal/manager/service.go:1201-1210

## Hiện tượng
Operator đổi `program` hoặc `pids` của input UDP multicast trên stream đang chạy, ví dụ chọn kênh khác trong MPTS. API trả 200 và cấu hình mới hiện ra, nhưng stream vẫn phát kênh cũ. Sau đó, kể cả khi failover về input này, manager vẫn dùng cấu hình cũ.

## Điều kiện kích hoạt
`PUT /streams/{code}` chỉ đổi `inputs[i].program` hoặc `inputs[i].pids`, URL giữ nguyên.

## Phân tích
```go
func inputsEqual(a, b domain.Input) bool {
    return a.URL == b.URL &&
        reflect.DeepEqual(a.Headers, b.Headers) &&
        reflect.DeepEqual(a.Params, b.Params) &&
        a.Net == b.Net
}
```
`domain.Input` còn có `Program` và `Pids`, và `NewPacketReader` dùng chúng để bọc reader UDP qua `maybeWrapMPTS` và `maybeWrapPIDs`. Khi chỉ hai trường này đổi, `UpdatedInputs` rỗng nên `UpdateInputs` không được gọi. `h.Input` trong manager giữ bản cũ, nên các lần `tryFailover` và probe sau đó vẫn dựng reader theo cấu hình cũ.

## Vì sao đây là lỗi
Đây đúng là lớp lỗi mà CLAUDE.md cảnh báo: trường không có trong diff thì không được áp dụng cho stream đang chạy. Các trường runtime như `Alive` không cần so sánh, nhưng `Program` và `Pids` là cấu hình persisted.

## Hướng sửa
Thêm `a.Program == b.Program && slices.Equal(a.Pids, b.Pids)` vào `inputsEqual`, và bổ sung case vào `diff_test.go`.

## Cách xác minh
Test `ComputeDiff` với hai input chỉ khác `Program`, sau đó chỉ khác `Pids`; kỳ vọng `UpdatedInputs` có 1 phần tử. Hiện tại rỗng.
