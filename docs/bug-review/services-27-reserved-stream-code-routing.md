# Stream code trùng tiền tố admin tạo được nhưng output HTTP không thể truy cập

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api, internal/domain, internal/autopublish
- **Vị trí:** internal/domain/stream.go:23-41, internal/api/server.go:227-313, internal/api/server.go:320-325, internal/api/dispatch.go:129-131

## Hiện tượng
Stream `vod/news` hoặc `hooks` chạy bình thường nhưng `/vod/news/index.m3u8` trả 404/405 của admin API, hoặc 401 khi bật auth. Viewer không xem được và API không cảnh báo lúc tạo stream.

## Điều kiện kích hoạt
`POST /streams/vod/news` hoặc code có đoạn đầu là `config`, `streams`, `templates`, `policies`, `sessions`, `hooks`, `watermarks`, `vod`. Auto-publish với prefix tương tự cũng tạo được code này.

## Phân tích
`ValidateStreamCode` chỉ kiểm tra ký tự và dấu `/`, không có danh sách từ khoá. Router đăng ký các subrouter admin, và chúng sở hữu toàn bộ cây con:
```go
ar.Route("/vod", func(r chi.Router) { ... })
...
mr.HandleFunc("/*", s.dispatchMedia())
```
Comment trong `buildRouter` xác nhận subrouter giữ request lại, không rơi xuống catch-all media. Comment của `dispatchMedia` cũng chỉ nói nó "only fires for paths not claimed by /streams, ...", tức xung đột là có thật nhưng không được chặn.

## Vì sao đây là lỗi
Hai không gian tên dùng chung đoạn đầu của URL, và trường hợp xung đột được chấp nhận khi lưu rồi chỉ lộ ra lúc phát.

## Hướng sửa
Từ chối đoạn đầu thuộc danh sách route admin trong `ValidateStreamCode`, có xử lý tương thích cho stream đã tồn tại; hoặc chuyển media sang tiền tố riêng.

## Cách xác minh
Test router: tạo server với handler stub, `GET /vod/news/index.m3u8` không đi vào `dispatchMedia`; `ValidateStreamCode("vod/news")` trả `nil`.
