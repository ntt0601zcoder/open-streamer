# Đổi program hoặc pids của input không áp dụng khi stream đang chạy

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/domain, internal/coordinator, internal/manager, internal/ingestor
- **Vị trí:** internal/domain/input.go:63, internal/domain/input.go:78, internal/coordinator/diff.go:98-100, internal/coordinator/diff.go:233-238, internal/ingestor/reader.go:163-177, internal/manager/service.go:1201-1209

## Hiện tượng
Operator đổi `program` để chọn kênh khác trong luồng MPTS, hoặc đổi `pids`. API trả 200 và hiện giá trị mới, nhưng kênh phát vẫn là kênh cũ. Lần restart tiếp theo kênh mới đột ngột có hiệu lực.

## Điều kiện kích hoạt
1. Stream UDP MPTS với `inputs[0].program: 101` đang chạy.
2. `POST /streams/{code}` đổi thành `program: 102`, URL giữ nguyên.

## Phân tích
`inputsEqual` không so `Program` và `Pids`:

```go
return a.URL == b.URL &&
    reflect.DeepEqual(a.Headers, b.Headers) &&
    reflect.DeepEqual(a.Params, b.Params) &&
    a.Net == b.Net
```

Do đó input không vào `UpdatedInputs`, `UpdateInputs` không được gọi và pull worker không restart. Hai trường này chỉ được đọc khi tạo reader (`input.Program`, `input.Pids` trong `reader.go`). `InputHealth.Input` trong manager cũng giữ bản cũ, nên failover hoặc probe về sau vẫn dùng giá trị cũ.

## Vì sao đây là lỗi
CLAUDE.md đã cảnh báo trường không được liệt kê trong `ComputeDiff` sẽ không áp dụng cho stream đang chạy. Bản ghi và runtime lệch nhau âm thầm.

## Hướng sửa
Thêm `a.Program == b.Program && slices.Equal(a.Pids, b.Pids)` vào `inputsEqual`.

## Cách xác minh
Thêm case vào `TestComputeDiff_InputUpdated` chỉ đổi `Program`. Thử thực tế với nguồn UDP MPTS: đổi `program` và xem log không có "manager: input updated".
