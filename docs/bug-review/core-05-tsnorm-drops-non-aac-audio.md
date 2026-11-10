# Đường raw-TS loại bỏ MP2/MP3/AC-3 và MPEG-2 video, kênh DVB mất tiếng hoặc mất hẳn output
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/tsnorm, ingestor/worker, domain
- **Vị trí:** internal/ingestor/tsnorm/tsnorm.go:614-627,488-491; internal/ingestor/worker.go:552-571; internal/domain/avpacket.go:25-35; docs/CODEC.md:38-41,64

## Hiện tượng
Nguồn UDP/HLS/SRT/HTTP-TS/file có audio MP2 (phổ biến trên DVB) hoặc AC-3: output không có tiếng. Kênh radio chỉ có MP2: không có packet nào vào buffer, dù manager vẫn báo input đang hoạt động và panel tracks vẫn hiện `mp2a` (vì StatsDemuxer đọc dữ liệu gốc). Nguồn MPEG-2 video mất hình.

## Điều kiện kích hoạt
Bất kỳ nguồn raw-TS nào có elementary stream khác H.264, H.265, AAC. `tsNormaliser` luôn khác nil trong worker.

## Phân tích
```go
switch st {
case astits.StreamTypeH264Video: ...
case astits.StreamTypeH265Video: ...
case astits.StreamTypeAACAudio: ...
default:
    return nil, false, false
}
```
Muxer đầu ra cũng chỉ có PID cho ba codec này. Khi cả chunk không tạo ra output, worker chạy nhánh:
```go
// All frames dropped ...
wctx.lastWriteAt.Store(...)
return nil
```
Nên stall watchdog cũng không phát hiện.

## Vì sao đây là lỗi
`domain.AVCodecMP2` ghi "Common in DVB radio channels", `tsmux.FromAV` hỗ trợ MP2/MP3, và docs/CODEC.md liệt kê `mp2a` là codec ingest được. Trước khi có tsnorm, TS được forward nguyên bản nên các codec này đi qua. Fallback passthrough chỉ chạy khi demux lỗi, không chạy khi codec không được hỗ trợ.

## Hướng sửa
Trong tsnorm, thêm đường cho MP2/MP3 (PID audio với `StreamTypeMPEG2Audio` và PMT tương ứng). Với codec chưa remux được (AC-3, MPEG-2 video), hoặc thêm stream type tương ứng, hoặc phát hiện từ PMT và chuyển stream sang passthrough, kèm log cảnh báo.

## Cách xác minh
Đưa file TS có H.264 + MP2 qua `tsnorm.Process`, demux output và kiểm tra có PES audio. Hoặc `ffprobe` output HLS của một nguồn UDP có MP2.
