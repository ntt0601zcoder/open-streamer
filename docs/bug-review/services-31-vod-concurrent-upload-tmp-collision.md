# Hai upload VOD cùng tên dùng chung file .tmp, có thể tạo file hỏng hoặc ghi đè

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler
- **Vị trí:** internal/api/handler/vod.go:375-386, internal/api/handler/vod.go:453-473

## Hiện tượng
Khi client thử lại một upload lớn trong lúc lần đầu còn chạy, file đích có thể là bản trộn của hai lần ghi, một request nhận lỗi rename, và request còn lại vẫn báo 201 với kích thước không khớp nội dung.

## Điều kiện kích hoạt
Hai `POST /vod/{name}/files` đồng thời với cùng `filename` và `path`.

## Phân tích
Cả hai đều vượt qua kiểm tra tồn tại vì file đích chưa có:
```go
if _, err := os.Stat(abs); err == nil {
```
rồi dùng chung đường dẫn tạm và mở bằng cờ truncate:
```go
tmp := dst + ".tmp"
out, err := os.Create(tmp)
```
Lần sau cắt file của lần trước. Hai FD ghi chồng offset lên cùng inode. Rename đầu tiên công bố file đang được ghi dở; rename thứ hai gặp `ENOENT` và `os.Remove(tmp)`. Nếu bên thứ hai rename sau khi tmp vẫn còn, file đích bị ghi đè dù API cam kết không.

## Vì sao đây là lỗi
Comment của `UploadFile` cam kết "Existing files are not overwritten" và của `writeUploadAtomic` nêu không để lại file dở. Watermark có mutex cho đúng tình huống này, VOD thì không.

## Hướng sửa
Dùng `os.CreateTemp` trong cùng thư mục và publish bằng `os.Link` (thất bại khi đích đã tồn tại) rồi xoá tạm, thay cho `Rename`.

## Cách xác minh
Test: hai goroutine gọi `writeUploadAtomic` cùng `dst` với nội dung khác nhau và reader chậm; kiểm tra nội dung đích bị lẫn hoặc có lỗi rename.
