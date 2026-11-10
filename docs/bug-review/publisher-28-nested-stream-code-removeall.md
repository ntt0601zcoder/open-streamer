# Dừng stream cha xóa luôn thư mục HLS/DASH của stream con
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher, domain, autopublish
- **Vị trí:** internal/publisher/service.go:427-442; internal/domain/stream.go:23-41; internal/publisher/hls.go:725; internal/publisher/fileutil.go:22-25

## Hiện tượng
Khi dừng hoặc reload stream `a/b`, stream đang chạy `a/b/c` mất playlist và segment. HLS của stream con ghi lỗi `HLS write segment failed` mãi cho tới khi nó restart.

## Điều kiện kích hoạt
Hai stream có code lồng nhau, ví dụ `a` và `a/b`, hoặc `region/north` và `region/north/live/foo`. Stream cha `Stop`, gồm cả `reloadTranscoderFull`. Stream code dạng `x/track_1` cũng trùng thư mục rendition của stream ABR `x`.

## Phân tích
`ValidateStreamCode` cho phép `/` và không chặn tiền tố. Thư mục được ghép thẳng từ code:
```go
p := filepath.Join(dir, string(streamID))
os.RemoveAll(p)
```
`writeFileAtomic` của HLS không tạo lại thư mục (`os.CreateTemp` lỗi ENOENT), còn DASH có `MkdirAll` nên tự phục hồi một phần với file mới.

## Vì sao đây là lỗi
ARCHITECTURE chủ động hỗ trợ code nhiều phân đoạn và auto-publish tạo các path như vậy. Không có kiểm tra xung đột giữa các code khi tạo thư mục, chỉ có kiểm tra chồng prefix giữa các template.

## Hướng sửa
Mã hóa code thành một tên thư mục không lồng (kèm thay đổi tương ứng ở mediaserve), hoặc khi xóa chỉ gỡ file của chính stream và bỏ qua thư mục con là stream khác; đồng thời tạo lại thư mục khi ghi HLS.

## Cách xác minh
Test: chạy `a/b/c`, gọi `Stop("a/b")`, kiểm tra thư mục con còn nguyên và segment kế tiếp ghi được.
