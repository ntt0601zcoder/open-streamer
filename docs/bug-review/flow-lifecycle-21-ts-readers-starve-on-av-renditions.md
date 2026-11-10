# copy:// và mixer:// đọc rendition transcode theo chế độ TS nên không nhận được gì

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder, internal/buffer, internal/ingestor/pull, internal/coordinator, internal/domain, internal/publisher
- **Vị trí:** internal/transcoder/supervisor.go:431-438, internal/ingestor/pull/buffer_ts_chunk.go:83-88, internal/ingestor/pull/copy.go:102-105, internal/ingestor/pull/mixer.go:149-156, internal/coordinator/abr_mixer.go:326, internal/coordinator/abr_mixer.go:222-223, internal/domain/copy_shape.go:181-187, internal/publisher/serve_mpegts.go:195-202

## Hiện tượng
Downstream phụ thuộc vào upstream có transcoder bị đen:
- `copy://X` có transcoder riêng: input timeout, chuyển exhausted rồi lặp lại.
- `mixer://X,Y` với X có ladder: không có hình. Với Y có transcoder: không có tiếng.
- ABR mixer vẫn báo stream Active, input Degraded.
- HTTP MPEG-TS của stream transcode trả 200 nhưng body rỗng.

ABR copy không dính vì chuyển nguyên `pkt`.

## Điều kiện kích hoạt
Cấu hình downstream `copy://` hoặc `mixer://` trỏ vào upstream đang transcode. Không cần race.

## Phân tích
Transcoder ghi AVPacket:
```go
bufPkt := buffer.Packet{AV: av, SessionStart: pkt.GetSessionStart()}
```
Rà toàn bộ `buf.Write`, chỉ ingestor raw-TS ghi `Packet.TS`. Nhưng consumer chọn chế độ TS cho rendition (`streamHasRenditions`, `StreamMainBufferIsTS` với `Transcoder != nil`), và chunk reader bỏ qua AV:
```go
if len(pkt.TS) == 0 {
    // AV-only packet ...
    continue
}
```
Nên demuxer không có byte nào. `abrMixerVideoForward` và nhánh audio cũng qua `NewBufferTSDemuxReader`. HTTP MPEG-TS cũng có `continue` với AV.

## Vì sao đây là lỗi
Doc của `StreamMainBufferIsTS` và comment nhiều nơi vẫn nói "transcoder output is TS". Commit `be219af` đã đưa transcoder sang AV-path nên giả định không còn đúng. Validator chấp nhận cấu hình và không có lỗi được báo; test hiện tại chỉ kiểm shape.

## Hướng sửa
Dùng nhánh direct-AV cho rendition (giữ TS demux chỉ cho nguồn raw-TS), hoặc cho `bufferTSChunkReader` remux AV bằng `tsmux.FeedWirePacket`. Cập nhật `StreamMainBufferIsTS` và handler MPEG-TS tương ứng.

## Cách xác minh
Unit test với buffer thật: ghi `Packet{AV: ...}` vào buffer rendition, mở `CopyReader` với upstream có ladder và khẳng định có packet. Trên môi trường thử: tạo `copy://` tới stream ABR kèm transcoder và xem `input.last_packet_at`.
