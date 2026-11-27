# tsnorm loại bỏ MP2, AC-3 và MPEG-2 khỏi đường raw-TS nên nguồn DVB mất tiếng hoặc mất cả stream
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/tsnorm, ingestor, ingestor/pull, manager
- **Vị trí:** internal/ingestor/tsnorm/tsnorm.go:429-435, 608-627, 469-492; internal/ingestor/worker.go:551-572; internal/ingestor/pull/stats_pmt_scanner.go:46-57; internal/domain/avpacket.go:25-59

## Hiện tượng
Feed UDP/SRT/HLS dùng audio MPEG-1/2 Layer II hoặc AC-3 (phổ biến trong DVB) phát ra không có tiếng, trong khi panel tracks của input vẫn hiện `mp2a` hoặc `ac3`. Feed video MPEG-2 cộng audio MP2 hoặc AC-3 không tạo ra output nào: stream bị timeout rồi Degraded dù gói vẫn tới đều.

## Điều kiện kích hoạt
Nguồn raw-TS (UDP, HLS, HTTP-TS, SRT, file) có elementary stream khác H.264, H.265, AAC-ADTS: stream_type 0x03/0x04 (MP2), 0x81 hoặc 0x06 + descriptor AC-3, 0x02 (MPEG-2), 0x11 (AAC-LATM).

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
Output chỉ là những gì `writeMuxed` nhận, và đầu ra chỉ có ba codec. Mọi PES khác biến mất mà không có log.

Khi cả chunk không tạo được byte nào, `writeRawTSChunk` đi vào nhánh:
```go
} else {
    // All frames dropped ...
    if wctx.lastWriteAt != nil { wctx.lastWriteAt.Store(time.Now().UnixNano()) }
    return nil
}
```
Nhánh này không ghi hub và không gọi `onPacket`, nên manager báo timeout. Việc bump `lastWriteAt` còn che luôn stall watchdog.

## Vì sao đây là lỗi
Phần còn lại của hệ thống coi các codec này là hợp lệ: `domain.AVCodecMP2` ghi "Common in DVB", `tsmux/fromav.go:256` có case MP2/MP3, `tsdemux_packet_reader.go` map 0x03/0x04, `stats_pmt_scanner.go` nhận diện AC-3 và MPEG-2. Trước khi có tsnorm, các byte này đi passthrough. Comment `//nolint:exhaustive` chỉ nhắc AV1/VP9/Opus, không nói việc mất MP2/AC-3 trên feed DVB.

## Hướng sửa
PID có codec không normalise được thì vẫn giữ: hoặc thêm PES passthrough với PTS được anchor theo cùng normaliser (tsmux đã hỗ trợ MP2), hoặc bỏ qua tsnorm cho stream đó và cảnh báo. Tối thiểu phải log Warn một lần cho mỗi PID bị bỏ. Không bump `lastWriteAt` khi không ghi gì.

## Cách xác minh
Unit test: nạp TS chứa PID MP2 vào `tsnorm.Process`, kiểm tra output không có PID audio. Thủ công: `ffmpeg -c:v libx264 -c:a mp2 -f mpegts udp://...`, chạy `ffprobe` trên HLS đầu ra.
