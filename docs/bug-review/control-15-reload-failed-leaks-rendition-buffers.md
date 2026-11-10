# reloadTranscoderFull lỗi làm rò rỉ rendition buffer mới
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** coordinator, buffer
- **Vị trí:** internal/coordinator/coordinator.go:649-655, 673-682, 693-698, 719-724, 518-524

## Hiện tượng
Sau một lần reload lỗi khi đổi ladder, Buffer Hub giữ các buffer `$r$<code>$track_N` không còn dùng. Metric buffer và số buffer tăng mãi cho tới khi restart process, nếu ladder sau đó có ít rung hơn hoặc stream bị xoá.

## Điều kiện kích hoạt
`PUT` đổi ladder trên stream đang chạy, và `tc.Start` thất bại, ví dụ binary transcoder vừa bị thiếu sau deploy.

## Phân tích
`reloadTranscoderFull` xoá `c.renditions[code]`, tạo buffer mới, rồi chỉ ghi lại slug sau khi `tc.Start` thành công:
```go
if err := c.tc.Start(...); err != nil {
    return c.reloadFailed(ctx, new.Code, fmt.Errorf("transcoder start: %w", err))
}
c.rendMu.Lock()
c.renditions[new.Code] = renditionSlugs
```
`reloadFailed` gọi `stopLocked`, nhưng `stopLocked` chỉ xoá các slug có trong `c.renditions`, lúc này đang rỗng.

## Vì sao đây là lỗi
`startLocked` có dọn slug khi `tc.Start` lỗi, còn đường reload thì không. Reconciler start lại stream thì `buf.Create` là idempotent nên dùng lại các buffer trùng tên, nhưng các rung thừa hoặc stream đã xoá vẫn bị giữ.

## Hướng sửa
Lưu `c.renditions[new.Code] = renditionSlugs` ngay sau khi tạo buffer, trước `tc.Start`, để `stopLocked` dọn được.

## Cách xác minh
Test coordinator: `tc` spy trả lỗi ở `Start` trong reload, sau đó kiểm tra `buf.Subscribe` trên `$r$code$track_1` trả lỗi "not found". Hiện tại subscribe thành công.
