# Đổi program hoặc pids của input không áp dụng cho stream đang chạy

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/coordinator, internal/manager, internal/ingestor
- **Vị trí:** internal/coordinator/diff.go:233-238, internal/domain/input.go:63, internal/domain/input.go:78, internal/manager/service.go:1201-1210, internal/ingestor/reader.go:100-103

## Hiện tượng
Operator đổi `program` hoặc `pids` trên input UDP (ví dụ chọn chương trình MPTS khác): `PUT` thành công nhưng output vẫn là chương trình cũ. Sau khi restart hoặc failover, cấu hình mới mới có hiệu lực, gây thay đổi nội dung bất ngờ.

## Điều kiện kích hoạt
`PUT` chỉ đổi `inputs[i].program` hoặc `inputs[i].pids`, URL giữ nguyên, stream đang chạy.

## Phân tích
So sánh input thiếu hai trường:
```go
return a.URL == b.URL &&
    reflect.DeepEqual(a.Headers, b.Headers) &&
    reflect.DeepEqual(a.Params, b.Params) &&
    a.Net == b.Net
```
Nên input không vào `UpdatedInputs` và `h.Input` trong manager giữ bản cũ. Hai trường này lại được `maybeWrapMPTS` và `maybeWrapPIDs` dùng khi tạo reader, và probe cùng failover dùng `h.Input` cũ tới khi pipeline restart.

## Vì sao đây là lỗi
`diffInputs` khẳng định phát hiện "same priority, different config". Không có guard khác cho hai trường này.

## Hướng sửa
Thêm `a.Program == b.Program` và so sánh `Pids` trong `inputsEqual`, kèm test diff.

## Cách xác minh
Unit test `ComputeDiff` với hai input khác `Program` hoặc `Pids`, khẳng định `UpdatedInputs` chứa input đó.
