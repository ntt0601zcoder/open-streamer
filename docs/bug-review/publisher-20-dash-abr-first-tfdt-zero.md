# Segment đầu của mọi shard ABR có tfdt=0 dù AST dùng chung
- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** publisher/dash
- **Vị trí:** internal/publisher/dash/packager.go:749-764, 922, 952, 1060-1063

## Hiện tượng
Khi một rendition bắt đầu muộn hơn rendition khác, nội dung cùng `S@t` giữa các Representation lệch nhau đúng khoảng trễ đó. Đổi chất lượng có thể bị nhảy hình.

## Điều kiện kích hoạt
Một shard flush segment đầu sau shard khác một khoảng đáng kể, ví dụ lỡ IDR đầu tiên nên lệch một GOP.

## Phân tích
`writeSegments` chạy trước khi `availStart` được gán:
```go
flushed := p.writeSegments(now, d)
...
if p.availStart.IsZero() { p.availStart = now; ... SharedAST(now) }
```
Trong lúc ghi, `wallclockTicks(now, p.availStart, ...)` nhận AST rỗng và trả 0. Vì vậy cả shard đầu lẫn shard muộn đều có tfdt=0, còn AST là của shard đầu.

## Vì sao đây là lỗi
Comment B-4 mong mọi shard neo về một AST chung, nhưng tfdt của shard muộn lại không dùng AST đó. Thường các rendition từ cùng một subprocess nên chênh nhỏ, vì thế mức độ thấp.

## Hướng sửa
Trong tryCut, lấy `SharedAST(now)` trước khi gọi `writeSegments` cho lần đầu để tfdt shard muộn là `now - AST`.

## Cách xác minh
Unit test với `ABRMaster` đã có AST cũ hơn 4 giây: segment đầu của shard mới phải có tfdt khoảng 4 x 90000.
