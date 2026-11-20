# Tắt HLS/DASH khi stream đang chạy để lại playlist cũ vẫn được phục vụ
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher, api/mediaserve
- **Vị trí:** internal/publisher/service.go:361-380, 472-489; internal/publisher/hls.go:130-139, 791; internal/api/dispatch.go:216-219

## Hiện tượng
Sau khi tắt HLS hoặc DASH trên stream đang chạy, URL `index.m3u8`/`index.mpd` vẫn trả 200 với nội dung đóng băng. Bật lại thì sequence quay về 0, player cũ bị rối và các segment số lớn của phiên trước còn nằm trên đĩa.

## Điều kiện kích hoạt
Cập nhật `protocols.hls` hoặc `protocols.dash` từ true thành false, có thể bật lại sau đó, trong khi stream vẫn chạy.

## Phân tích
`UpdateProtocols` gọi `stopProtocol`, chỉ hủy goroutine. `cleanupStreamDirs` chỉ chạy trong `Stop`. `dispatchMedia` phục vụ file theo đường dẫn mà không kiểm tra giao thức có bật hay không:
```go
roots.ServeFile(w, r, code, file)
```
Khi bật lại, segmenter mới có `segN=0`, `onDisk` rỗng, nên ghi đè `seg_000001.ts` và các file cũ số lớn không bao giờ bị xóa cho đến `Stop`.

## Vì sao đây là lỗi
Người vận hành tắt giao thức thì viewer vẫn nhận playlist cũ như đang live, và `MEDIA-SEQUENCE` giảm vi phạm yêu cầu tăng đơn điệu của HLS khi bật lại.

## Hướng sửa
Sau khi goroutine thoát, xóa thư mục output của giao thức đó. Hoặc giữ `segN` tăng tiếp qua các lần bật tắt.

## Cách xác minh
Test `UpdateProtocols` HLS true sang false: sau khi goroutine kết thúc, thư mục HLS phải bị xóa hoặc endpoint trả 404.
