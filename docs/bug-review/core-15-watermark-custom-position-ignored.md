# Watermark position=custom và resize được chấp nhận nhưng bị bỏ qua
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** domain, transcoder
- **Vị trí:** internal/domain/watermark.go:33,98-99,112,190-193; internal/transcoder/supervisor.go:672-686; internal/transcoder/native/watermark.go:20-35,261-300

## Hiện tượng
Đặt `position=custom` với `x`/`y`: watermark luôn nằm góc trên bên trái kèm offset mặc định 10 px, không báo lỗi. Đặt `resize=true`: watermark không co theo rung.

## Điều kiện kích hoạt
Stream có watermark với `position: custom` hoặc `resize: true`.

## Phân tích
`WatermarkConfig.Validate` yêu cầu `x`/`y` khi `custom`, nhưng `watermarkConfigFromTC` không truyền `X`, `Y`. Struct native không có các field này. `resolveTextPosition` và `resolveOverlayPosition` không có case `custom`:
```go
default: // top_left
    return ox, oy
```
`Resolved()` còn đặt offset mặc định 10 vì `Position != Center`. `Resize` được đưa vào proto nhưng grep trong `internal/transcoder/native` (trừ `.pb.go`) không có code dùng.

## Vì sao đây là lỗi
API và doc của field mô tả biểu thức tùy ý, kể cả fade theo thời gian. Lỗi không được báo cho operator.

## Hướng sửa
Truyền X/Y qua proto và xử lý case `custom` trong hai hàm resolve. X/Y phải được kiểm tra hoặc escape vì đi thẳng vào filter graph (không cho `:`, `;`, `,`, `[`, `'` ngoài biểu thức hợp lệ). Hiện thực `Resize` hoặc gỡ field.

## Cách xác minh
Unit test `BuildWatermarkFilter` với `position=custom, x="100", y="50"`, kiểm tra chuỗi filter có `x=100:y=50`.
