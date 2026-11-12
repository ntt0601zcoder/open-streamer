# Dạng buffer chỉ suy ra từ Inputs[0], failover sang input khác kiểu làm copy:// và mixer:// mất dữ liệu
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** domain, ingestor/pull, coordinator
- **Vị trí:** internal/domain/copy_shape.go:181-191; internal/ingestor/pull/copy.go:102-105,157-161; internal/ingestor/pull/buffer_ts_chunk.go:82-87; internal/coordinator/coordinator.go:770

## Hiện tượng
Upstream có input chính RTMP/RTSP và input dự phòng UDP/HLS (hoặc ngược lại). Khi upstream failover, stream `copy://upstream` hoặc `mixer://` mất toàn bộ dữ liệu cho đến khi upstream quay lại input chính. DVR ghi rỗng trong khoảng này mà status vẫn là "recording".

## Điều kiện kích hoạt
1. Upstream có Inputs[0] và Inputs[1] khác nhóm (AV so với raw-TS).
2. Manager chuyển sang input kia.

## Phân tích
```go
return inputSourceIsRawTS(s.Inputs[0].URL)
```
Reader quyết định mode một lần lúc khởi tạo. Mode direct bỏ packet TS (`if pkt.AV == nil { return nil, nil }`), mode TS bỏ packet AV (`if len(pkt.TS) == 0 { continue }`).

Coordinator cũng chỉ xét `StreamMainBufferIsTS` để từ chối DVR cho nguồn raw-TS, nên nguồn chính AV và dự phòng raw-TS vẫn được ghi, nhưng writer bỏ qua `pkt.TS`.

## Vì sao đây là lỗi
`ValidateCopyShape` cho phép rõ ràng "Single-stream copy + network fallback" với lý do "publishers see a single TS source regardless of which input is active", điều này không đúng với upstream có input hỗn hợp. Không có thay đổi mode khi failover vì failover của upstream không tạo sự kiện nào tới reader của downstream.

## Hướng sửa
Reader xử lý cả hai dạng packet trên cùng một subscriber (TS thì đưa qua demuxer, AV thì đi thẳng). Cùng hướng sửa với core-01. DVR cần xử lý `pkt.TS` hoặc từ chối khi bất kỳ input nào là raw-TS.

## Cách xác minh
Tạo `CopyReader` trên buffer main với upstream Inputs[0]=rtmp, ghi `buffer.Packet{TS: ...}` và kiểm tra reader có nhận được packet nào.
