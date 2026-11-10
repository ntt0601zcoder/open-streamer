# File tạm multipart của upload không bao giờ bị xoá, mỗi upload VOD làm đầy /tmp thêm một lần

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, net/http, chi
- **Vị trí:** internal/api/handler/vod.go:331-342, internal/api/handler/watermark.go:157-168, internal/api/server.go:203

## Hiện tượng
Sau mỗi upload VOD lớn, trong `os.TempDir()` còn lại file `multipart-*` cỡ bằng file đã upload (tới 10 GiB). Đĩa tạm đầy dần và chỉ giải phóng khi dọn tay. Khi `/tmp` nằm cùng phân vùng với HLS/DVR, ghi segment cũng lỗi theo.

## Điều kiện kích hoạt
`POST /vod/{name}/files` với file lớn hơn `uploadMemoryBytes` (32 MiB). Watermark cũng bị khi lớn hơn 4 MiB.

## Phân tích
net/http chỉ dọn form trên request gốc:
```go
if w.req.MultipartForm != nil {
    w.req.MultipartForm.RemoveAll()
}
```
Handler nhận bản sao vì `chi.Mux.ServeHTTP` và `middleware.Timeout` đều gọi `r.WithContext(...)`, mà hàm này copy struct (`r2 := new(Request); *r2 = *r`). `r.ParseMultipartForm` gán `MultipartForm` lên bản sao, nên request gốc của server vẫn có `MultipartForm == nil`.

Cả hai handler không gọi `r.MultipartForm.RemoveAll()`; grep toàn repo không có lời gọi nào.

## Vì sao đây là lỗi
`writeUploadAtomic` chép sang đích nhưng phần spool vẫn còn. Cơ chế dọn tự động của net/http không chạy do request bị copy.

## Hướng sửa
Thêm `defer func(){ if r.MultipartForm != nil { _ = r.MultipartForm.RemoveAll() } }()` ngay sau `ParseMultipartForm` ở cả hai handler. Với VOD nên dùng `r.MultipartReader()` để stream thẳng tới đích, tránh ghi hai lần.

## Cách xác minh
Test tích hợp: chạy handler qua `chi` + `middleware.Timeout`, đặt `TMPDIR` riêng, upload file lớn hơn ngưỡng bộ nhớ, kiểm tra thư mục còn `multipart-*` sau khi response kết thúc.
