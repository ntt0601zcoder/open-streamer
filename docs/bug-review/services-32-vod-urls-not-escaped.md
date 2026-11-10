# PlayURL và IngestURL của VOD hỏng với tên file có ký tự đặc biệt

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/vod, internal/api/handler
- **Vị trí:** internal/vod/registry.go:279-280, internal/vod/registry.go:185-206, internal/api/handler/vod.go:393-394

## Hiện tượng
File như `Tap #1.mp4`, `100%.mp4` hoặc `a?b.mp4` được liệt kê với URL dùng nguyên văn, nhưng dán `ingest_url` vào input thì báo không tìm thấy file hoặc lỗi parse; `play_url` mở trong trình duyệt trả 404.

## Điều kiện kích hoạt
Upload hoặc đặt file có `#`, `?`, `%` trong tên, rồi dùng `ingest_url`/`play_url` trả về.

## Phân tích
URL được ghép chuỗi:
```go
entry.PlayURL = "/vod/" + string(name) + "/raw/" + rel
entry.IngestURL = "file://" + string(name) + "/" + rel
```
Đầu đọc lại dùng `url.Parse`:
```go
u, err := url.Parse(rawURL)
...
rel := strings.TrimPrefix(u.Path, "/")
```
`#1.mp4` thành fragment nên path thiếu phần đuôi, `?` thành query, `%.m` là escape không hợp lệ và parse lỗi.

## Vì sao đây là lỗi
Comment của `ListFiles` nói mỗi entry mang URL "the client can hand back to the ingestor as-is". `UploadFile` nhận các tên này mà không kiểm tra.

## Hướng sửa
Escape từng đoạn path bằng `url.PathEscape` khi dựng hai URL, ở cả `ListFiles` và `UploadFile`.

## Cách xác minh
Unit test: dựng `FileEntry` cho `a#b.mp4`, đưa `IngestURL` vào `Registry.Resolve`, kiểm tra path trả về thiếu `#b.mp4`.
