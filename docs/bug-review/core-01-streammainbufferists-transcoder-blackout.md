# copy:// và mixer:// từ upstream có transcoder không nhận được packet nào
- **Mức độ:** Nghiêm trọng
- **Độ tin cậy:** Cao
- **Module liên quan:** domain, ingestor/pull, coordinator (abr_mixer), transcoder (supervisor)
- **Vị trí:** internal/domain/copy_shape.go:181-191; internal/transcoder/supervisor.go:438; internal/ingestor/pull/buffer_ts_chunk.go:82-87; internal/ingestor/pull/copy.go:102-105; internal/ingestor/pull/mixer.go:149-156; internal/coordinator/abr_mixer.go:223,333

## Hiện tượng
Stream dùng `copy://X` (khi có transcoder riêng) hoặc `mixer://X,Y` với upstream có transcoder không ra hình hoặc tiếng. Không có log lỗi. Sau `input_packet_timeout_sec` input bị đánh dấu Degraded, dù upstream vẫn phát bình thường.

## Điều kiện kích hoạt
1. Upstream X có `transcoder` (ladder ABR, hoặc chỉ re-encode audio).
2. Downstream dùng một trong ba cấu hình:
   - `copy://X` và có transcoder riêng.
   - `mixer://X,...` đi qua `MixerReader`.
   - `mixer://` với video upstream là ABR và không có transcoder, đi qua `abrMixerVideoForward`.

Biến thể thứ hai: upstream có `transcoder` với `video.copy=true` và `audio.copy=true`, nguồn RTMP/RTSP. Không có subprocess nào chạy và main buffer chứa AV, nhưng `StreamMainBufferIsTS` vẫn trả về `true`.

## Phân tích
`StreamMainBufferIsTS` vẫn giả định transcoder xuất TS:
```go
if s.Transcoder != nil { return true }
```
Trong khi nơi duy nhất ghi vào rendition buffer là supervisor, và nó ghi AV:
```go
bufPkt := buffer.Packet{AV: av, SessionStart: pkt.GetSessionStart()}
```
Grep toàn bộ `buffer.Packet{` cho thấy `TS:` chỉ được ghi ở `ingestor/worker.go` vào `bufferWriteID` (raw hoặc main), không bao giờ vào `$r$...`.

Reader chọn nhánh TS vì `streamHasRenditions(up) || StreamMainBufferIsTS(up)`, và `bufferTSChunkReader.Read` bỏ qua mọi packet không có TS:
```go
if len(pkt.TS) == 0 { continue }
```
Nên reader lặp vô hạn trên các packet AV và không trả về byte nào.

## Vì sao đây là lỗi
Comment ở copy_shape.go ("buffer holds the transcoder's stdout TS", "rendition buffers always hold TS") đã lỗi thời từ khi transcoder chuyển sang gRPC với AVPacket. Test `copy_test` tự tạo buffer theo giả định cũ nên không phát hiện. Đường `abrCopyTapForward` không bị ảnh hưởng vì nó forward nguyên packet, do đó chỉ một số topology bị đen hình.

## Hướng sửa
Reader nên xử lý theo dạng packet thực tế thay vì đoán từ config: nhận cả `pkt.AV` và `pkt.TS` trên cùng subscriber. Nếu giữ phân loại tĩnh thì `StreamMainBufferIsTS` và `streamHasRenditions` phải phản ánh đúng việc rendition chứa AV, và trường hợp cả hai copy phải xét theo nguồn.

## Cách xác minh
Test tích hợp: tạo rendition buffer, ghi `buffer.Packet{AV: ...}` như supervisor, mở `NewCopyReader` với upstream có `Transcoder` và kiểm tra `ReadPackets` có trả packet. Thực tế: tạo stream A có ladder, stream B `mixer://A,A2` rồi quan sát `$r$`/buffer của B.
