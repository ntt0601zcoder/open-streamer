# copy:// và mixer:// không dùng được với upstream là stream auto-publish

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/ingestor/pull, internal/domain, internal/coordinator, cmd/server, internal/autopublish
- **Vị trí:** cmd/server/main.go:404-419, internal/coordinator/coordinator.go:113-129, internal/ingestor/pull/copy.go:86-89, internal/domain/copy_shape.go:69-71, internal/domain/copy_shape.go:108-111, internal/api/handler/stream.go:491-499

## Hiện tượng
Stream `copy://region/north/foo` trỏ tới stream runtime đang phát: API lưu thành công, nhưng downstream luôn Degraded với lỗi `upstream stream "..." not found`, dù buffer upstream đang có dữ liệu. `GET /streams` lại hiển thị upstream đó đang chạy.

## Điều kiện kích hoạt
1. Encoder đẩy tạo stream runtime `region/north/foo`.
2. Tạo stream B với input `copy://region/north/foo`.

## Phân tích
Cả hai đường lookup chỉ đọc store, mà stream runtime không bao giờ nằm trong store:

```go
ing.SetStreamLookup(func(code domain.StreamCode) (*domain.Stream, bool) {
    s, err := repo.FindByCode(context.Background(), code)
    if err != nil {
        return nil, false
    }
```

`NewCopyReader` trả lỗi khi không thấy. `upstreamLookup` của coordinator cũng tương tự nên `detectABRCopy` không nhận ra ladder. Validate thì coi upstream thiếu là "shape unknown" nên cho qua.

## Vì sao đây là lỗi
Stream runtime được đưa ra như một stream hợp lệ trên API. Việc lưu thành công nhưng không thể chạy là một ca "validation accepts configs a later module cannot run", và lookup không phản ánh trạng thái runtime đã biết của autopublish.

## Hướng sửa
Trong lookup, khi store không có thì hỏi autopublish `Lookup(code)` rồi resolve theo template, dựng stream ảo. Nếu không hỗ trợ, tài liệu hoá và từ chối ở validate khi code khớp một prefix.

## Cách xác minh
Test lookup với autopublish giả có entry và repo rỗng: `NewCopyReader` phải thành công.
