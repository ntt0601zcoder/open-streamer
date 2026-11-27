# runtime.media sai với stream kế thừa transcoder từ template

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/domain, internal/autopublish
- **Vị trí:** internal/api/handler/stream.go:150, internal/api/handler/stream.go:192-198, internal/api/handler/stream.go:306, internal/api/handler/stream.go:331

## Hiện tượng
Stream lấy ladder từ template, hoặc stream runtime: `GET /streams` báo `runtime.media.outputs` bằng đúng track đầu vào và bitrate đầu vào, trong khi thực tế đang phát ABR nhiều rendition. UI hiển thị sai đầu ra.

## Điều kiện kích hoạt
Stream có `template: t`, `transcoder` để trống, template có 3 profile. Stream đang chạy, gọi `GET /streams/{code}`.

## Phân tích
`withStatus` truyền bản ghi thô vào summary:

```go
rt.Media = buildMediaSummary(s, rt.Inputs, rt.ActiveInputPriority)
```

```go
if s.Transcoder != nil {
    out.Outputs = domain.OutputTracks(s.Transcoder)
    ...
} else {
    out.Outputs = out.Inputs
```

Bản thô có `Transcoder == nil` khi kế thừa, và stub runtime cũng vậy. Comment "No transcode → output is whatever the publisher re-muxes" không còn đúng trong trường hợp này.

## Vì sao đây là lỗi
Quy tắc trả bản ghi thô áp dụng cho phần cấu hình. Phần `runtime` được định nghĩa là trạng thái đang chạy, nên phải dựa trên cấu hình đã resolve.

## Hướng sửa
Trong `withStatus`, gọi `h.resolveStream` chỉ để tính `buildMediaSummary`, vẫn trả `Stream` thô ở phần cấu hình. Giữ cho ABR copy nhánh dùng ladder của upstream.

## Cách xác minh
Test handler: stream kế thừa transcoder 2 profile, `Runtime.Media.Outputs` phải có track của profile.
