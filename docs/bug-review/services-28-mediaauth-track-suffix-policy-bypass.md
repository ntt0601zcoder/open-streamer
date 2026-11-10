# Stream có code kết thúc bằng track_N bị kiểm tra policy theo code cha

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api, internal/mediaauth, internal/domain
- **Vị trí:** internal/api/dispatch.go:30-32, internal/api/dispatch.go:44-54, internal/domain/stream.go:23-41, cmd/server/main.go:370-382

## Hiện tượng
Stream `cam/track_1` gắn policy yêu cầu token vẫn phát được trên HTTP mà không có token, nếu không có stream `cam` hoặc stream `cam` là công khai. Token ký đúng cho `cam/track_1` lại bị từ chối khi `cam` có policy.

## Điều kiện kích hoạt
Tạo stream có đoạn cuối dạng `track_<số>` với policy, hoặc auto-publish path như vậy, rồi mở `/cam/track_1/index.m3u8`.

## Phân tích
```go
Code: stripABRTrackSlug(code),
```
Đoạn cuối bị bỏ mà không kiểm tra có stream mang đúng code đó không. `resolvePolicy("cam")` không thấy stream nên trả `""`, và authorizer cho phép. RTMP/SRT/RTSP không dùng cắt này, nên hai đường cho kết quả khác nhau.

## Vì sao đây là lỗi
Comment gọi slug là "closed internal namespace", nhưng `ValidateStreamCode` không cấm `track_<N>`. Kết quả là bỏ qua policy của chính stream.

## Hướng sửa
Cấm đoạn `track_<số>` trong stream code, hoặc chỉ strip khi `code` không phải stream đang tồn tại và code cha có rendition đó.

## Cách xác minh
Test `mediaAllowed` với resolver map: `cam/track_1` có policy token, `cam` không có, gọi không token, kiểm tra hiện trả `true`.
